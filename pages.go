package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	com "github.com/dbhubio/common"
	sqlite "github.com/gwenn/gosqlite"
	"github.com/icza/session"
	"github.com/jackc/pgx"
)

func databasePage(w http.ResponseWriter, r *http.Request, userName string, dbName string, dbTable string) {
	pageName := "Render database page"

	var pageData struct {
		Meta com.MetaInfo
		DB   com.SQLiteDBinfo
		Data com.SQLiteRecordSet
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
		pageData.Meta.LoggedInUser = loggedInUser
	}

	// Check if the user has access to the requested database
	err := com.CheckUserDBAccess(&pageData.DB, loggedInUser, userName, dbName)
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// * Execution can only get here if the user has access to the requested database *

	// Generate a predictable cache key for the whole page data
	var pageCacheKey string
	if loggedInUser != userName {
		tempArr := md5.Sum([]byte(userName + "/" + dbName + "/" + dbTable))
		pageCacheKey = "dwndb-pub-" + hex.EncodeToString(tempArr[:])
	} else {
		tempArr := md5.Sum([]byte(loggedInUser + "-" + userName + "/" + dbName + "/" + dbTable))
		pageCacheKey = "dwndb-" + hex.EncodeToString(tempArr[:])
	}

	// Determine the number of rows to display
	if loggedInUser != "" {
		pageData.DB.MaxRows = com.PrefUserMaxRows(loggedInUser)
	} else {
		// Not logged in, so default to 10 rows
		pageData.DB.MaxRows = 10
	}

	// If a cached version of the page data exists, use it
	pageCacheKey += "/" + strconv.Itoa(pageData.DB.MaxRows)
	ok, err := com.GetCachedData(pageCacheKey, &pageData)
	if err != nil {
		log.Printf("%s: Error retrieving page data from cache: %v\n", pageName, err)
	}
	if ok {
		// Render the page from cache
		t := tmpl.Lookup("databasePage")
		err = t.Execute(w, pageData)
		if err != nil {
			log.Printf("Error: %s", err)
		}
		return
	}

	// Get a handle from Minio for the database object
	db, err := com.OpenMinioObject(pageData.DB.MinioBkt, pageData.DB.MinioId)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	defer db.Close()

	// Retrieve the list of tables in the database
	tables, err := db.Tables("")
	if err != nil {
		log.Printf("Error retrieving table names: %s", err)
		// TODO: Add proper error handing here.  Maybe display the page, but show the error where
		// TODO  the table data would otherwise be?
		errorPage(w, r, http.StatusInternalServerError,
			fmt.Sprintf("Error reading from '%s'.  Possibly encrypted or not a database?", dbName))
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("The database '%s' doesn't seem to have any tables. Aborting.", dbName)
		errorPage(w, r, http.StatusInternalServerError, "Database has no tables?")
		return
	}
	pageData.DB.Info.Tables = tables

	// If a specific table was requested, check that it's present
	if dbTable != "" {
		// Check the requested table is present
		tablePresent := false
		for _, tbl := range tables {
			if tbl == dbTable {
				tablePresent = true
			}
		}
		if tablePresent == false {
			// The requested table doesn't exist in the database
			log.Printf("%s: Requested table not present in database. DB: '%s/%s', Table: '%s'\n", pageName,
				userName, dbName, dbTable)
			errorPage(w, r, http.StatusBadRequest, "Requested table not present")
			return
		}
	}

	// If a specific table wasn't requested, use the first table in the database
	if dbTable == "" {
		dbTable = pageData.DB.Info.Tables[0]
	}

	// Retrieve (up to) x rows from the selected database
	// Ugh, have to use string smashing for this, even though the SQL spec doesn't seem to say table names
	// shouldn't be parameterised.  Limitation from SQLite's implementation? :(
	stmt, err := db.Prepare("SELECT * FROM "+dbTable+" LIMIT ?", pageData.DB.MaxRows)
	if err != nil {
		log.Printf("Error when preparing statement for database: %s\v", err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}

	// Retrieve the field names
	pageData.Data.ColNames = stmt.ColumnNames()
	pageData.Data.ColCount = len(pageData.Data.ColNames)

	// Process each row
	fieldCount := -1
	err = stmt.Select(func(s *sqlite.Stmt) error {

		// Get the number of fields in the result
		if fieldCount == -1 {
			fieldCount = stmt.DataCount()
		}

		// Retrieve the data for each row
		var row []com.DataValue
		for i := 0; i < fieldCount; i++ {
			// Retrieve the data type for the field
			fieldType := stmt.ColumnType(i)

			isNull := false
			switch fieldType {
			case sqlite.Integer:
				var val int
				val, isNull, err = s.ScanInt(i)
				if err != nil {
					log.Printf("Something went wrong with ScanInt(): %v\n", err)
					break
				}
				if !isNull {
					stringVal := fmt.Sprintf("%d", val)
					row = append(row, com.DataValue{Name: pageData.Data.ColNames[i],
						Type: com.Integer, Value: stringVal})
				}
			case sqlite.Float:
				var val float64
				val, isNull, err = s.ScanDouble(i)
				if err != nil {
					log.Printf("Something went wrong with ScanDouble(): %v\n", err)
					break
				}
				if !isNull {
					stringVal := strconv.FormatFloat(val, 'f', 4, 64)
					row = append(row, com.DataValue{Name: pageData.Data.ColNames[i],
						Type: com.Float, Value: stringVal})
				}
			case sqlite.Text:
				var val string
				val, isNull = s.ScanText(i)
				if !isNull {
					row = append(row, com.DataValue{Name: pageData.Data.ColNames[i],
						Type: com.Text, Value: val})
				}
			case sqlite.Blob:
				_, isNull = s.ScanBlob(i)
				if !isNull {
					row = append(row, com.DataValue{Name: pageData.Data.ColNames[i],
						Type: com.Binary, Value: "<i>BINARY DATA</i>"})
				}
			case sqlite.Null:
				isNull = true
			}
			if isNull {
				row = append(row, com.DataValue{Name: pageData.Data.ColNames[i], Type: com.Null,
					Value: "<i>NULL</i>"})
			}
		}
		pageData.Data.Records = append(pageData.Data.Records, row)

		return nil
	})
	if err != nil {
		log.Printf("Error when retrieving select data from database: %s\v", err)
		errorPage(w, r, http.StatusInternalServerError,
			fmt.Sprintf("Error reading data from '%s'.  Possibly malformed?", dbName))
		return
	}
	defer stmt.Finalize()

	// Count the total number of rows in the selected table
	dbQuery := "SELECT count(*) FROM " + dbTable
	err = db.OneValue(dbQuery, &pageData.Data.RowCount)
	if err != nil {
		log.Printf("%s: Error occurred when counting total table rows: %s\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failure")
		return
	}

	pageData.Data.Tablename = dbTable
	pageData.Meta.Username = userName
	pageData.Meta.Database = dbName
	pageData.Meta.Server = com.WebServer()
	pageData.Meta.Title = fmt.Sprintf("%s / %s", userName, dbName)

	// Cache the page data
	err = com.CacheData(pageCacheKey, pageData, com.CacheTime)
	if err != nil {
		log.Printf("%s: Error when caching page data: %v\n", pageName, err)
	}

	// TODO: Should we cache the rendered page too?

	// Render the page
	t := tmpl.Lookup("databasePage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

// General error display page
func errorPage(w http.ResponseWriter, r *http.Request, httpcode int, msg string) {
	var pageData struct {
		Meta    com.MetaInfo
		Message string
	}
	pageData.Message = msg

	// Retrieve session data (if any)
	sess := session.Get(r)
	if sess != nil {
		loggedInUser := sess.CAttr("UserName")
		pageData.Meta.LoggedInUser = fmt.Sprintf("%s", loggedInUser)
	}

	// Render the page
	w.WriteHeader(httpcode)
	t := tmpl.Lookup("errorPage")
	err := t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

// Renders the front page of the website
func frontPage(w http.ResponseWriter, r *http.Request) {
	// Structure to hold page data
	var pageData struct {
		Meta com.MetaInfo
		List []com.UserInfo
	}

	// Retrieve session data (if any)
	sess := session.Get(r)
	if sess != nil {
		loggedInUser := sess.CAttr("UserName")
		pageData.Meta.LoggedInUser = fmt.Sprintf("%s", loggedInUser)
	}

	// Retrieve list of users with public databases
	var err error
	pageData.List, err = com.PublicUserDBs()
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	pageData.Meta.Title = `SQLite storage "in the cloud"`

	// Render the page
	t := tmpl.Lookup("rootPage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func loginPage(w http.ResponseWriter, r *http.Request) {
	var pageData struct {
		Meta      com.MetaInfo
		SourceURL string
	}
	pageData.Meta.Title = "Login"

	// Retrieve session data (if any)
	sess := session.Get(r)
	if sess != nil {
		loggedInUser := sess.CAttr("UserName")
		pageData.Meta.LoggedInUser = fmt.Sprintf("%s", loggedInUser)
	}

	// If the referrer is a page from our website, pass that to the login page
	referrer := r.Referer()
	if referrer != "" {
		ref, err := url.Parse(referrer)
		if err != nil {
			log.Printf("Error when parsing referrer URL for login page: %s\n", err)
		} else {
			// localhost:8080 means the server is running on a local (development) box ;)
			if ref.Host == "localhost:8080" || strings.HasSuffix(ref.Host, "dbhub.io") {
				pageData.SourceURL = ref.Path
			}
		}
	}

	// Render the page
	t := tmpl.Lookup("loginPage")
	err := t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

// Renders the user Preferences page
func prefPage(w http.ResponseWriter, r *http.Request, userName string) {
	pageName := "Preference page form"

	var pageData struct {
		Meta    com.MetaInfo
		MaxRows int
	}
	pageData.Meta.Title = "Preferences"
	pageData.Meta.LoggedInUser = userName

	// Retrieve the user preference data
	dbQuery := `
		SELECT pref_max_rows
		FROM users
		WHERE username = $1`
	err := db.QueryRow(dbQuery, userName).Scan(&pageData.MaxRows)
	if err != nil {
		log.Printf("%s: Error retrieving User preference data: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Error retrieving preference data")
		return
	}

	// Render the page
	t := tmpl.Lookup("prefPage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func profilePage(w http.ResponseWriter, r *http.Request, userName string) {
	pageName := "User Page"

	// Structure to hold page data
	type starRow struct {
		Username    string
		Database    string
		DateStarred time.Time
	}
	var pageData struct {
		Meta       com.MetaInfo
		PrivateDBs []com.DBInfo
		PublicDBs  []com.DBInfo
		Stars      []starRow
	}
	pageData.Meta.Username = userName
	pageData.Meta.Title = userName
	pageData.Meta.Server = com.WebServer()
	pageData.Meta.LoggedInUser = userName

	// Check if the desired user exists
	row := db.QueryRow("SELECT count(username) FROM public.users WHERE username = $1", userName)
	var userCount int
	err := row.Scan(&userCount)
	if err != nil {
		log.Printf("%s: Error looking up user details failed. User: '%s' Error: %v\n", pageName, userName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}

	// If the user doesn't exist, display an error page
	if userCount == 0 {
		errorPage(w, r, http.StatusNotFound, fmt.Sprintf("Unknown user: %s", userName))
		return
	}

	var dbQuery string
	// Retrieve list of public databases for the user
	dbQuery = `
		WITH public_dbs AS (
			SELECT db.dbname, db.last_modified, ver.size, ver.version, db.watchers, db.stars,
				db.forks, db.discussions, db.pull_requests, db.updates, db.branches,
				db.releases, db.contributors, db.description
			FROM sqlite_databases AS db, database_versions AS ver
			WHERE db.idnum = ver.db
				AND db.username = $1
				AND ver.public = true
			ORDER BY dbname, version DESC
		), unique_dbs AS (
			SELECT DISTINCT ON (dbname) * FROM public_dbs ORDER BY dbname
		)
		SELECT * FROM unique_dbs ORDER BY last_modified DESC`
	rows, err := db.Query(dbQuery, userName)
	if err != nil {
		log.Printf("%s: Database query failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var desc pgx.NullString
		var oneRow com.DBInfo
		err = rows.Scan(&oneRow.Database, &oneRow.LastModified, &oneRow.Size, &oneRow.Version,
			&oneRow.Watchers, &oneRow.Stars, &oneRow.Forks, &oneRow.Discussions, &oneRow.MRs,
			&oneRow.Updates, &oneRow.Branches, &oneRow.Releases, &oneRow.Contributors, &desc)
		if err != nil {
			log.Printf("%s: Error retrieving public database list for user: %v\n", pageName, err)
			errorPage(w, r, http.StatusInternalServerError, "Error retrieving database list")
			return
		}
		if !desc.Valid {
			oneRow.Description = ""
		} else {
			oneRow.Description = fmt.Sprintf(": %s", desc.String)
		}
		pageData.PublicDBs = append(pageData.PublicDBs, oneRow)
	}

	// Retrieve list of private databases for the user
	dbQuery = `
		WITH public_dbs AS (
			SELECT db.dbname, db.last_modified, ver.size, ver.version, db.watchers, db.stars,
				db.forks, db.discussions, db.pull_requests, db.updates, db.branches,
				db.releases, db.contributors, db.description
			FROM sqlite_databases AS db, database_versions AS ver
			WHERE db.idnum = ver.db
				AND db.username = $1
				AND ver.public = false
			ORDER BY dbname, version DESC
		), unique_dbs AS (
			SELECT DISTINCT ON (dbname) * FROM public_dbs ORDER BY dbname
		)
		SELECT * FROM unique_dbs ORDER BY last_modified DESC`
	rows2, err := db.Query(dbQuery, userName)
	if err != nil {
		log.Printf("%s: Database query failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows2.Close()
	for rows2.Next() {
		var desc pgx.NullString
		var oneRow com.DBInfo
		err = rows2.Scan(&oneRow.Database, &oneRow.LastModified, &oneRow.Size, &oneRow.Version,
			&oneRow.Watchers, &oneRow.Stars, &oneRow.Forks, &oneRow.Discussions, &oneRow.MRs,
			&oneRow.Updates, &oneRow.Branches, &oneRow.Releases, &oneRow.Contributors, &desc)
		if err != nil {
			log.Printf("%s: Error retrieving private database list for user: %v\n", pageName, err)
			errorPage(w, r, http.StatusInternalServerError, "Error retrieving database list")
			return
		}
		if !desc.Valid {
			oneRow.Description = ""
		} else {
			oneRow.Description = fmt.Sprintf(": %s", desc.String)
		}
		pageData.PrivateDBs = append(pageData.PrivateDBs, oneRow)
	}

	// Retrieve the list of starred databases for the user
	dbQuery = `
		WITH stars AS (
			SELECT db, date_starred
			FROM database_stars
			WHERE username = $1
		)
		SELECT dbs.username, dbs.dbname, stars.date_starred
		FROM sqlite_databases AS dbs, stars
		WHERE dbs.idnum = stars.db
		ORDER BY date_starred DESC`
	rows3, err := db.Query(dbQuery, userName)
	if err != nil {
		log.Printf("%s: Database query failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows3.Close()
	for rows3.Next() {
		var oneRow starRow
		err = rows3.Scan(&oneRow.Username, &oneRow.Database, &oneRow.DateStarred)
		if err != nil {
			log.Printf("%s: Error retrieving stars list for user: %v\n", pageName, err)
			errorPage(w, r, http.StatusInternalServerError, "Error retrieving stars list")
			return
		}
		pageData.Stars = append(pageData.Stars, oneRow)
	}

	// Render the page
	t := tmpl.Lookup("profilePage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func registerPage(w http.ResponseWriter, r *http.Request) {
	var pageData struct {
		Meta com.MetaInfo
	}
	pageData.Meta.Title = "Register"

	// Retrieve session data (if any)
	sess := session.Get(r)
	if sess != nil {
		loggedInUser := sess.CAttr("UserName")
		pageData.Meta.LoggedInUser = fmt.Sprintf("%s", loggedInUser)
	}

	// Render the page
	t := tmpl.Lookup("registerPage")
	err := t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func starsPage(w http.ResponseWriter, r *http.Request, userName string, dbName string) {
	pageName := "Stars page"

	type userInfo struct {
		Username    string
		DateStarred time.Time
	}
	var pageData struct {
		Meta  com.MetaInfo
		Stars []userInfo
	}
	pageData.Meta.Title = "Stars"
	pageData.Meta.Username = userName
	pageData.Meta.Database = dbName

	// Retrieve session data (if any)
	sess := session.Get(r)
	if sess != nil {
		loggedInUser := sess.CAttr("UserName")
		pageData.Meta.LoggedInUser = fmt.Sprintf("%s", loggedInUser)
	}

	// Retrieve list of users who starred the database
	dbQuery := `
		WITH star_users AS (
			SELECT DISTINCT ON (username) username, date_starred
			FROM database_stars
			WHERE db = (
				SELECT idnum
				FROM sqlite_databases
				WHERE username = $1
					AND dbname = $2
				)
			ORDER BY username DESC
		)
		SELECT username, date_starred
		FROM star_users
		ORDER BY date_starred DESC`
	rows, err := db.Query(dbQuery, userName, dbName)
	if err != nil {
		log.Printf("%s: Database query failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var oneRow userInfo
		err = rows.Scan(&oneRow.Username, &oneRow.DateStarred)
		if err != nil {
			log.Printf("%s: Error retrieving list of stars for %s/%s: %v\n", pageName, userName, dbName,
				err)
			errorPage(w, r, http.StatusInternalServerError, "Database query failed")
			return
		}
		pageData.Stars = append(pageData.Stars, oneRow)
	}

	// Render the page
	t := tmpl.Lookup("starsPage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func uploadPage(w http.ResponseWriter, r *http.Request, userName string) {
	var pageData struct {
		Meta com.MetaInfo
	}
	pageData.Meta.Title = "Upload database"
	pageData.Meta.LoggedInUser = userName

	// Render the page
	t := tmpl.Lookup("uploadPage")
	err := t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func userPage(w http.ResponseWriter, r *http.Request, userName string) {
	pageName := "User Page"

	// Structure to hold page data
	var pageData struct {
		Meta   com.MetaInfo
		DBRows []com.DBInfo
	}
	pageData.Meta.Username = userName
	pageData.Meta.Title = userName
	pageData.Meta.Server = com.WebServer()

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
		if loggedInUser == userName {
			// The logged in user is looking at their own user page
			profilePage(w, r, loggedInUser)
			return
		}
		pageData.Meta.LoggedInUser = loggedInUser
	}

	// Check if the desired user exists
	row := db.QueryRow("SELECT count(username) FROM public.users WHERE username = $1", userName)
	var userCount int
	err := row.Scan(&userCount)
	if err != nil {
		log.Printf("%s: Error looking up user details failed. User: '%s' Error: %v\n", pageName, userName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}

	// If the user doesn't exist, display an error page
	if userCount == 0 {
		errorPage(w, r, http.StatusNotFound, fmt.Sprintf("Unknown user: %s", userName))
		return
	}

	var dbQuery string
	// Retrieve list of public databases for the user
	dbQuery = `
		WITH public_dbs AS (
			SELECT db.dbname, db.last_modified, ver.size, ver.version, db.watchers, db.stars, db.forks,
				db.discussions, db.pull_requests, db.updates, db.branches, db.releases,
				db.contributors, db.description
			FROM sqlite_databases AS db, database_versions AS ver
			WHERE db.idnum = ver.db
				AND db.username = $1
				AND ver.public = true
			ORDER BY dbname, version DESC
		), unique_dbs AS (
			SELECT DISTINCT ON (dbname) * FROM public_dbs ORDER BY dbname
		)
		SELECT * FROM unique_dbs ORDER BY last_modified DESC`
	rows, err := db.Query(dbQuery, userName)
	if err != nil {
		log.Printf("%s: Database query failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var desc pgx.NullString
		var oneRow com.DBInfo
		err = rows.Scan(&oneRow.Database, &oneRow.LastModified, &oneRow.Size, &oneRow.Version,
			&oneRow.Watchers, &oneRow.Stars, &oneRow.Forks, &oneRow.Discussions, &oneRow.MRs,
			&oneRow.Updates, &oneRow.Branches, &oneRow.Releases, &oneRow.Contributors, &desc)
		if err != nil {
			log.Printf("%s: Error retrieving database list for user: %v\n", pageName, err)
			errorPage(w, r, http.StatusInternalServerError, "Error retrieving database list for user")
			return
		}
		if !desc.Valid {
			oneRow.Description = ""
		} else {
			oneRow.Description = fmt.Sprintf(": %s", desc.String)
		}
		pageData.DBRows = append(pageData.DBRows, oneRow)
	}

	// Render the page
	t := tmpl.Lookup("userPage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}

func visualisePage(w http.ResponseWriter, r *http.Request) {
	// Structure to hold page data
	var pageData struct {
		Meta     com.MetaInfo
		DB       com.SQLiteDBinfo
		Data     com.SQLiteRecordSet
		ColNames []string
	}
	pageData.Meta.Title = "Visualise data"

	// Retrieve user and database name
	userName, dbName, requestedTable, err := com.GetODT(1, r)
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}
	pageData.Meta.Username = userName
	pageData.Meta.Database = dbName

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
		pageData.Meta.LoggedInUser = loggedInUser
	}

	// Check if the user has access to the requested database
	err = com.CheckUserDBAccess(&pageData.DB, loggedInUser, pageData.Meta.Username, dbName)
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Get a handle from Minio for the database object
	db, err := com.OpenMinioObject(pageData.DB.MinioBkt, pageData.DB.MinioId)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	defer db.Close()

	// Retrieve the list of tables in the database
	tables, err := db.Tables("")
	if err != nil {
		log.Printf("Error retrieving table names: %s", err)
		// TODO: Add proper error handing here.  Maybe display the page, but show the error where
		// TODO  the table data would otherwise be?
		errorPage(w, r, http.StatusInternalServerError,
			fmt.Sprintf("Error reading from '%s'.  Possibly encrypted or not a database?", dbName))
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("The database '%s' doesn't seem to have any tables. Aborting.", dbName)
		errorPage(w, r, http.StatusInternalServerError, "Database has no tables?")
		return
	}
	pageData.DB.Info.Tables = tables

	// If a specific table was requested, check it exists
	if requestedTable != "" {
		tablePresent := false
		for _, tableName := range tables {
			if requestedTable == tableName {
				tablePresent = true
			}
		}
		if tablePresent == false {
			// The requested table doesn't exist
			errorPage(w, r, http.StatusBadRequest, "Requested table does not exist")
			return
		}
	}

	// If no specific table was requested, just choose the first one given to us in the list from the database
	if requestedTable == "" {
		requestedTable = tables[0]
	}
	pageData.Data.Tablename = requestedTable

	// Retrieve a list of all column names in the specified table
	var tempStruct com.SQLiteRecordSet
	tempStruct, err = com.ReadSQLiteDB(db, requestedTable, 1)
	if err != nil {
		// Some kind of error when reading the database data
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}
	pageData.ColNames = tempStruct.ColNames

	// TODO: If a full visualisation profile was specified, we should gather the data for it and provide it to the
	// TODO  render function

	// Read all of the data from the requested (or default) table, add it to the page data
	pageData.Data, err = com.ReadSQLiteDB(db, requestedTable, 1000) // 1000 row maximum for now
	if err != nil {
		// Some kind of error when reading the database data
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Render the page
	t := tmpl.Lookup("visualisePage")
	err = t.Execute(w, pageData)
	if err != nil {
		log.Printf("Error: %s", err)
	}
}
