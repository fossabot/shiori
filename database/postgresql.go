package database

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"code.techknowlogick.com/techknowlogick/shiori/model"
	_ "github.com/lib/pq" // db driver
	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/bcrypt"
)

// PostgreSQLDatabase implements the database interface for connection to a Postgresql database
type PostgreSQLDatabase struct {
	sqlx.DB
}

// OpenPostgreSQLDatabase creates and opens a connection the PostgreSQL Database
func OpenPostgreSQLDatabase(host, username, password, dbname string) (*PostgreSQLDatabase, error) {
	var err error
	connString := fmt.Sprintf("%s:%s@/%s", username, password, dbname)
	db := sqlx.MustConnect("postgresql", connString)
	db.SetMaxOpenConns(100)
	db.SetConnMaxLifetime(time.Second)

	tx := db.MustBegin()

	// Make sure to rollback if panic ever happened
	defer func() {
		if r := recover(); r != nil {
			panicErr, _ := r.(error)
			fmt.Println("Database error:", panicErr)
			tx.Rollback()

			db = nil
			err = panicErr
		}
	}()

	tables := []string{`
	CREATE TABLE IF NOT EXISTS account(
		id INTEGER PRIMARY KEY AUTO_INCREMENT,
		username VARCHAR(250) UNIQUE NOT NULL,
		password VARCHAR(100) NOT NULL
	)
	`, `
	CREATE TABLE IF NOT EXISTS bookmark( 
		id INTEGER PRIMARY KEY AUTO_INCREMENT,
		url VARCHAR(512) UNIQUE NOT NULL,
		title TEXT NOT NULL,
		image_url TEXT NOT NULL, 
		excerpt TEXT NOT NULL, 
		author TEXT NOT NULL,
		min_read_time INTEGER NOT NULL DEFAULT 0, 
		max_read_time INTEGER NOT NULL DEFAULT 0, 
		modified TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)
	`, `
	CREATE TABLE IF NOT EXISTS tag( 
		id INTEGER PRIMARY KEY AUTO_INCREMENT, 
		name VARCHAR(512) UNIQUE NOT NULL
	)
	`, `
	CREATE TABLE IF NOT EXISTS bookmark_tag(
		bookmark_id INTEGER NOT NULL,
		tag_id INTEGER NOT NULL, 
		PRIMARY KEY(bookmark_id,tag_id),
		FOREIGN KEY (tag_id) REFERENCES tag(id),
		FOREIGN KEY(bookmark_id) REFERENCES bookmark(id)
	)
	`, `
	CREATE TABLE IF NOT EXISTS bookmark_content (
		docid INTEGER NOT NULL,
		title TEXT,
		content TEXT,
		html TEXT,
		FULLTEXT(title,content),
		FOREIGN KEY(docid) REFERENCES bookmark(id)
	)
	`,
	}

	for _, table := range tables {
		tx.MustExec(table)
	}
	err = tx.Commit()
	checkError(err)

	return &PostgreSQLDatabase{*db}, nil
}


// CreateBookmark saves new bookmark to database. Returns new ID and error if any happened.
func (db *PostgreSQLDatabase) InsertBookmark(bookmark model.Bookmark) (bookmarkID int, err error) {
	// Check URL and title
	if bookmark.URL == "" {
		return -1, fmt.Errorf("URL must not be empty")
	}

	if bookmark.Title == "" {
		return -1, fmt.Errorf("Title must not be empty")
	}

	if bookmark.Modified == "" {
		bookmark.Modified = time.Now().UTC().Format("2006-01-02 15:04:05")
	}

	// Prepare transaction
	tx, err := db.Beginx()
	if err != nil {
		return -1, err
	}

	// Make sure to rollback if panic ever happened
	defer func() {
		if r := recover(); r != nil {
			panicErr, _ := r.(error)
			tx.Rollback()

			bookmarkID = -1
			err = panicErr
		}
	}()

	// Save article to database
	res := tx.MustExec(`INSERT INTO bookmark (
		url, title, image_url, excerpt, author, 
		min_read_time, max_read_time, modified) 
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`,
		bookmark.URL,
		bookmark.Title,
		bookmark.ImageURL,
		bookmark.Excerpt,
		bookmark.Author,
		bookmark.MinReadTime,
		bookmark.MaxReadTime,
		bookmark.Modified)

	// Get last inserted ID
	bookmarkID64, err := res.LastInsertId()
	checkError(err)
	bookmarkID = int(bookmarkID64)

	// Save bookmark content
	tx.MustExec(`INSERT INTO bookmark_content 
		(docid, title, content, html) VALUES (?, ?, ?, ?)`,
		bookmarkID, bookmark.Title, bookmark.Content, bookmark.HTML)

	// Save tags
	stmtGetTag, err := tx.Preparex(`SELECT id FROM tag WHERE name = ?`)
	checkError(err)

	stmtInsertTag, err := tx.Preparex(`INSERT INTO tag (name) VALUES (?)`)
	checkError(err)

	stmtInsertBookmarkTag, err := tx.Preparex(`INSERT IGNORE INTO bookmark_tag (tag_id, bookmark_id) VALUES (?, ?)`)
	checkError(err)

	for _, tag := range bookmark.Tags {
		tagName := strings.ToLower(tag.Name)
		tagName = strings.TrimSpace(tagName)

		tagID := -1
		err = stmtGetTag.Get(&tagID, tagName)
		checkError(err)

		if tagID == -1 {
			res := stmtInsertTag.MustExec(tagName)
			tagID64, err := res.LastInsertId()
			checkError(err)
			tagID = int(tagID64)
		}

		stmtInsertBookmarkTag.Exec(tagID, bookmarkID)
	}

	// Commit transaction
	err = tx.Commit()
	checkError(err)

	return bookmarkID, err
}

// GetBookmarks fetch list of bookmarks based on submitted indices.
func (db *PostgreSQLDatabase) GetBookmarks(withContent bool, ids ...int) ([]model.Bookmark, error) {

	// Prepare where clause
	args := []interface{}{}
	whereClause := " WHERE 1"

	if len(ids) > 0 {
		whereClause = " WHERE id IN ("
		for _, id := range ids {
			args = append(args, id)
			whereClause += "?,"
		}

		whereClause = whereClause[:len(whereClause)-1]
		whereClause += ")"
	}

	// Fetch bookmarks
	query := `SELECT id, 
		url, title, image_url, excerpt, author, 
		min_read_time, max_read_time, modified
		FROM bookmark` + whereClause

	bookmarks := []model.Bookmark{}
	err := db.Select(&bookmarks, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	// Fetch tags and contents for each bookmarks
	stmtGetTags, err := db.Preparex(`SELECT t.id, t.name 
		FROM bookmark_tag bt LEFT JOIN tag t ON bt.tag_id = t.id
		WHERE bt.bookmark_id = ? ORDER BY t.name`)
	if err != nil {
		return nil, err
	}

	stmtGetContent, err := db.Preparex(`SELECT title, content, html FROM bookmark_content WHERE docid = ?`)
	if err != nil {
		return nil, err
	}

	defer stmtGetTags.Close()
	defer stmtGetContent.Close()

	for i, book := range bookmarks {
		book.Tags = []model.Tag{}
		err = stmtGetTags.Select(&book.Tags, book.ID)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}

		if withContent {
			err = stmtGetContent.Get(&book, book.ID)
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
		}

		bookmarks[i] = book
	}

	return bookmarks, nil
}

// DeleteBookmarks removes all record with matching indices from database.
func (db *PostgreSQLDatabase) DeleteBookmarks(ids ...int) (err error) {

	// Create args and where clause
	args := []interface{}{}
	whereClause := " WHERE 1"

	if len(ids) > 0 {
		whereClause = " WHERE id IN ("
		for _, id := range ids {
			args = append(args, id)
			whereClause += "?,"
		}

		whereClause = whereClause[:len(whereClause)-1]
		whereClause += ")"
	}

	// Begin transaction
	tx, err := db.Beginx()
	if err != nil {
		return err
	}

	// Make sure to rollback if panic ever happened
	defer func() {
		if r := recover(); r != nil {
			panicErr, _ := r.(error)
			tx.Rollback()

			err = panicErr
		}
	}()

	// Delete bookmarks
	whereTagClause := strings.Replace(whereClause, "id", "bookmark_id", 1)
	whereContentClause := strings.Replace(whereClause, "id", "docid", 1)

	tx.MustExec("DELETE FROM bookmark "+whereClause, args...)
	tx.MustExec("DELETE FROM bookmark_tag "+whereTagClause, args...)
	tx.MustExec("DELETE FROM bookmark_content "+whereContentClause, args...)

	// Commit transaction
	err = tx.Commit()
	checkError(err)

	return err
}

// SearchBookmarks search bookmarks by the keyword or tags.
func (db *PostgreSQLDatabase) SearchBookmarks(orderLatest bool, keyword string, tags ...string) ([]model.Bookmark, error) {
	// Create initial variable
	keyword = strings.TrimSpace(keyword)
	whereClause := "WHERE 1"
	args := []interface{}{}

	// Create where clause for keyword
	if keyword != "" {
		whereClause += ` AND (url LIKE ? OR id IN (
			SELECT docid FROM bookmark_content 
			WHERE MATCH(title,content) AGAINST (?) )
		)`
		args = append(args, "%"+keyword+"%", keyword)
	}

	// Create where clause for tags
	if len(tags) > 0 {
		whereTagClause := ` AND id IN (
			SELECT bookmark_id FROM bookmark_tag 
			WHERE tag_id IN (SELECT id FROM tag WHERE name IN (`

		for _, tag := range tags {
			args = append(args, tag)
			whereTagClause += "?,"
		}

		whereTagClause = whereTagClause[:len(whereTagClause)-1]
		whereTagClause += `)) GROUP BY bookmark_id HAVING COUNT(bookmark_id) >= ?)`
		args = append(args, len(tags))

		whereClause += whereTagClause
	}

	// Search bookmarks
	query := `SELECT id, 
		url, title, image_url, excerpt, author, 
		min_read_time, max_read_time, modified
		FROM bookmark ` + whereClause

	if orderLatest {
		query += ` ORDER BY id DESC`
	}

	bookmarks := []model.Bookmark{}
	err := db.Select(&bookmarks, query, args...)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	// Fetch tags for each bookmarks
	stmtGetTags, err := db.Preparex(`SELECT t.id, t.name 
		FROM bookmark_tag bt LEFT JOIN tag t ON bt.tag_id = t.id
		WHERE bt.bookmark_id = ? ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer stmtGetTags.Close()

	for i := range bookmarks {
		tags := []model.Tag{}
		err = stmtGetTags.Select(&tags, bookmarks[i].ID)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}

		bookmarks[i].Tags = tags
	}

	return bookmarks, nil
}

// UpdateBookmarks updates the saved bookmark in database.
func (db *PostgreSQLDatabase) UpdateBookmarks(bookmarks ...model.Bookmark) (result []model.Bookmark, err error) {
	// Prepare transaction
	tx, err := db.Beginx()
	if err != nil {
		return []model.Bookmark{}, err
	}

	// Make sure to rollback if panic ever happened
	defer func() {
		if r := recover(); r != nil {
			panicErr, _ := r.(error)
			tx.Rollback()

			result = []model.Bookmark{}
			err = panicErr
		}
	}()

	// Prepare statement
	stmtUpdateBookmark, err := tx.Preparex(`UPDATE bookmark SET
		url = ?, title = ?, image_url = ?, excerpt = ?, author = ?,
		min_read_time = ?, max_read_time = ?, modified = ? WHERE id = ?`)
	checkError(err)

	stmtUpdateBookmarkContent, err := tx.Preparex(`UPDATE bookmark_content SET
		title = ?, content = ?, html = ? WHERE docid = ?`)
	checkError(err)

	stmtGetTag, err := tx.Preparex(`SELECT id FROM tag WHERE name = ?`)
	checkError(err)

	stmtInsertTag, err := tx.Preparex(`INSERT INTO tag (name) VALUES (?)`)
	checkError(err)

	stmtInsertBookmarkTag, err := tx.Preparex(`INSERT IGNORE INTO bookmark_tag (tag_id, bookmark_id) VALUES (?, ?)`)
	checkError(err)

	stmtDeleteBookmarkTag, err := tx.Preparex(`DELETE FROM bookmark_tag WHERE bookmark_id = ? AND tag_id = ?`)
	checkError(err)

	result = []model.Bookmark{}
	for _, book := range bookmarks {
		stmtUpdateBookmark.MustExec(
			book.URL,
			book.Title,
			book.ImageURL,
			book.Excerpt,
			book.Author,
			book.MinReadTime,
			book.MaxReadTime,
			book.Modified,
			book.ID)

		stmtUpdateBookmarkContent.MustExec(
			book.Title,
			book.Content,
			book.HTML,
			book.ID)

		newTags := []model.Tag{}
		for _, tag := range book.Tags {
			if tag.Deleted {
				stmtDeleteBookmarkTag.MustExec(book.ID, tag.ID)
				continue
			}

			if tag.ID == 0 {
				tagID := -1
				err = stmtGetTag.Get(&tagID, tag.Name)
				checkError(err)

				if tagID == -1 {
					res := stmtInsertTag.MustExec(tag.Name)
					tagID64, err := res.LastInsertId()
					checkError(err)
					tagID = int(tagID64)
				}

				stmtInsertBookmarkTag.Exec(tagID, book.ID)
			}

			newTags = append(newTags, tag)
		}

		book.Tags = newTags
		result = append(result, book)
	}

	// Commit transaction
	err = tx.Commit()
	checkError(err)

	return result, err
}

// CreateAccount saves new account to database. Returns new ID and error if any happened.
func (db *PostgreSQLDatabase) CreateAccount(username, password string) (err error) {
	// Hash password with bcrypt
	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(password), 10)
	if err != nil {
		return err
	}

	// Insert account to database
	_, err = db.Exec(`INSERT INTO account
		(username, password) VALUES (?, ?)`,
		username, hashedPassword)

	return err
}

// GetAccount fetch account with matching username
func (db *PostgreSQLDatabase) GetAccount(username string) (model.Account, error) {
	account := model.Account{}
	err := db.Get(&account,
		`SELECT id, username, password FROM account WHERE username = ?`,
		username)
	return account, err
}

// GetAccounts fetch list of accounts in database
func (db *PostgreSQLDatabase) GetAccounts(keyword string) ([]model.Account, error) {
	query := `SELECT id, username, password FROM account`
	args := []interface{}{}
	if keyword != "" {
		if false {
			query += ` WHERE username = ?`
			args = append(args, keyword)
		} else {
			query += ` WHERE username LIKE ?`
			args = append(args, "%"+keyword+"%")
		}
	}
	query += ` ORDER BY username`

	accounts := []model.Account{}
	err := db.Select(&accounts, query, args...)
	return accounts, err
}

// DeleteAccounts removes all record with matching usernames
func (db *PostgreSQLDatabase) DeleteAccounts(usernames ...string) error {
	// Prepare where clause
	args := []interface{}{}
	whereClause := " WHERE 1"

	if len(usernames) > 0 {
		whereClause = " WHERE username IN ("
		for _, username := range usernames {
			args = append(args, username)
			whereClause += "?,"
		}

		whereClause = whereClause[:len(whereClause)-1]
		whereClause += ")"
	}

	// Delete usernames
	_, err := db.Exec(`DELETE FROM account `+whereClause, args...)
	return err
}

// GetTags fetch list of tags and their frequency
func (db *PostgreSQLDatabase) GetTags() ([]model.Tag, error) {
	tags := []model.Tag{}
	query := `SELECT bt.tag_id id, t.name, COUNT(bt.tag_id) n_bookmarks 
		FROM bookmark_tag bt 
		LEFT JOIN tag t ON bt.tag_id = t.id
		GROUP BY bt.tag_id ORDER BY t.name`

	err := db.Select(&tags, query)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}

	return tags, nil
}

// GetNewID creates new ID for specified table
func (db *PostgreSQLDatabase) GetNewID(table string) (int, error) {
	var tableID int
	query := fmt.Sprintf(`SELECT IFNULL(MAX(id) + 1, 1) FROM %s`, table)

	err := db.Get(&tableID, query)
	if err != nil && err != sql.ErrNoRows {
		return -1, err
	}

	return tableID, nil
}

// GetBookmarkID fetchs bookmark ID based by its url
func (db *PostgreSQLDatabase) GetBookmarkID(url string) int {
	var bookmarkID int
	db.Get(&bookmarkID, `SELECT id FROM bookmark WHERE url = ?`, url)
	return bookmarkID
}
