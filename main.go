package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	mathrand "math/rand"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	com "github.com/dbhubio/common"
	sqlite "github.com/gwenn/gosqlite"
	"github.com/icza/session"
	"github.com/jackc/pgx"
	"github.com/minio/minio-go"
	"golang.org/x/crypto/bcrypt"
)

var (
	// Our configuration info
	//conf com.TomlConfig

	// PostgreSQL configuration info
	//pgConfig = new(pgx.ConnConfig)

	// Log file for incoming HTTPS requests
	reqLog *os.File

	// Our parsed HTML templates
	tmpl *template.Template
)

func downloadCSVHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Download CSV"

	// Extract the username, database, table, and version requested
	dbOwner, dbName, dbTable, dbVersion, err := com.GetODTV(2, r) // 2 = Ignore "/x/download/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Abort if no table name was given
	if dbTable == "" {
		log.Printf("%s: No table name given\n", pageName)
		errorPage(w, r, http.StatusBadRequest, "No table name given")
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
	}

	// Verify the given database exists and is ok to be downloaded (and get the Minio bucket + id while at it)
	bucket, id, err := com.MinioID(dbOwner, dbName, dbVersion, loggedInUser)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Get a handle from Minio for the database object
	sdb, err := com.OpenMinioObject(bucket, id)
	if err != nil {
		log.Printf("%s: Error retrieving DB from Minio: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}

	// Retrieve all of the data from the selected database table
	stmt, err := sdb.Prepare("SELECT * FROM " + dbTable)
	if err != nil {
		log.Printf("Error when preparing statement for database: %s\v", err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}

	// Process each row
	fieldCount := -1
	var resultSet [][]string
	err = stmt.Select(func(s *sqlite.Stmt) error {

		// Get the number of fields in the result
		if fieldCount == -1 {
			fieldCount = stmt.DataCount()
		}

		// Retrieve the data for each row
		var row []string
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
					row = append(row, fmt.Sprintf("%d", val))
				}
			case sqlite.Float:
				var val float64
				val, isNull, err = s.ScanDouble(i)
				if err != nil {
					log.Printf("Something went wrong with ScanDouble(): %v\n", err)
					break
				}
				if !isNull {
					row = append(row, strconv.FormatFloat(val, 'f', 4, 64))
				}
			case sqlite.Text:
				var val string
				val, isNull = s.ScanText(i)
				if !isNull {
					row = append(row, val)
				}
			case sqlite.Blob:
				var val []byte
				val, isNull = s.ScanBlob(i)
				if !isNull {
					// Base64 encode the value
					row = append(row, base64.StdEncoding.EncodeToString(val))
				}
			case sqlite.Null:
				isNull = true
			}
			if isNull {
				row = append(row, "NULL")
			}
		}
		resultSet = append(resultSet, row)

		return nil
	})
	if err != nil {
		log.Printf("Error when reading data from database: %s\v", err)
		errorPage(w, r, http.StatusInternalServerError,
			fmt.Sprintf("Error reading data from '%s'.  Possibly malformed?", dbName))
		return
	}
	defer stmt.Finalize()

	// Convert resultSet into CSV and send to the user
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.csv", url.QueryEscape(dbTable)))
	w.Header().Set("Content-Type", "text/csv")
	csvFile := csv.NewWriter(w)
	err = csvFile.WriteAll(resultSet)
	if err != nil {
		log.Printf("%s: Error when generating CSV: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Error when generating CSV")
		return
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Download Handler"

	dbOwner, dbName, dbVersion, err := com.GetODV(2, r) // 2 = Ignore "/x/download/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
	}

	// Verify the given database exists and is ok to be downloaded (and get the Minio bucket + id while at it)
	bucket, id, err := com.MinioID(dbOwner, dbName, dbVersion, loggedInUser)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Get a handle from Minio for the database object
	userDB, err := com.MinioHandle(bucket, id)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Close the object handle when this function finishes
	defer func() {
		com.MinioHandleClose(userDB)
	}()

	// Send the database to the user
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", url.QueryEscape(dbName)))
	w.Header().Set("Content-Type", "application/x-sqlite3")
	bytesWritten, err := io.Copy(w, userDB)
	if err != nil {
		log.Printf("%s: Error returning DB file: %v\n", pageName, err)
		fmt.Fprintf(w, "%s: Error returning DB file: %v\n", pageName, err)
		return
	}

	// Log the number of bytes written
	log.Printf("%s: '%s/%s' downloaded. %d bytes", pageName, dbOwner, dbName, bytesWritten)
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Login page"

	// TODO: Add browser side validation of the form data too to save a trip to the server
	// TODO  and make for a nicer user experience for sign up

	// Get the username, password, and referrer
	userName, password, bounceURL, err := com.GetUPS(r)
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, "Error when parsing login data")
		return
	}

	// Check if the required form data was submitted
	if userName == "" && password == "" {
		// No, so render the login page
		loginPage(w, r)
		return
	}

	// Retrieve the password hash for the user, if they exist in the database
	passHash, err := com.UserPasswordHash(userName)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}

	// Hash the user's password
	err = bcrypt.CompareHashAndPassword(passHash, []byte(password))
	if err != nil {
		log.Printf("%s: Login failure, username/password not correct. User: '%s'\n", pageName, userName)
		errorPage(w, r, http.StatusBadRequest, fmt.Sprint("Login failed. Username/password not correct"))
		return
	}

	// Create session cookie
	sess := session.NewSessionOptions(&session.SessOptions{
		CAttrs: map[string]interface{}{"UserName": userName},
	})
	session.Add(sess, w)

	if bounceURL == "" || bounceURL == "/register" || bounceURL == "/login" {
		// Bounce to the user's own page
		http.Redirect(w, r, "/"+userName, http.StatusTemporaryRedirect)
	} else {
		// Bounce to the original referring page
		http.Redirect(w, r, bounceURL, http.StatusTemporaryRedirect)
	}
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	// Remove session info
	sess := session.Get(r)
	if sess != nil {
		// Session data was present, so remove it
		session.Remove(sess, w)
	}

	// Bounce to the front page
	// TODO: This should probably reload the existing page instead
	http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
}

// Wrapper function to log incoming https requests
func logReq(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Check if user is logged in
		var loggedInUser string
		sess := session.Get(r)
		if sess == nil {
			loggedInUser = "-"
		} else {
			loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
		}

		// Write request details to the request log
		fmt.Fprintf(reqLog, "%v - %s [%s] \"%s %s %s\" \"-\" \"-\" \"%s\" \"%s\"\n", r.RemoteAddr,
			loggedInUser, time.Now().Format(time.RFC3339Nano), r.Method, r.URL, r.Proto,
			r.Referer(), r.Header.Get("User-Agent"))

		// Call the original function
		fn(w, r)
	}
}

func main() {
	// Read server configuration
	var err error
	if err = com.ReadConfig(); err != nil {
		log.Fatalf("Configuration file problem\n\n%v", err)
	}

	// Open the request log for writing
	reqLog, err = os.OpenFile(conf.Web.RequestLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY|os.O_SYNC, 0750)
	if err != nil {
		log.Fatalf("Error when opening request log: %s\n", err)
	}
	defer reqLog.Close()
	log.Printf("Request log opened: %s\n", conf.Web.RequestLog)

	// Setup session storage
	session.Global.Close()
	session.Global = session.NewCookieManagerOptions(session.NewInMemStore(),
		&session.CookieMngrOptions{AllowHTTP: false})

	// Parse our template files
	tmpl = template.Must(template.New("templates").Delims("[[", "]]").ParseGlob("templates/*.html"))

	// Connect to Minio server
	minioClient, err = minio.New(conf.Minio.Server, conf.Minio.AccessKey, conf.Minio.Secret, conf.Minio.HTTPS)
	if err != nil {
		log.Fatalf("Problem with Minio server configuration: \n\n%v", err)
	}

	// Log Minio server end point
	log.Printf("Minio server config ok. Address: %v\n", conf.Minio.Server)

	// Connect to PostgreSQL server
	db, err = pgx.Connect(*pgConfig)
	defer db.Close()
	if err != nil {
		log.Fatalf("Couldn't connect to database\n\n%v", err)
	}

	// Log successful connection message
	log.Printf("Connected to PostgreSQL server: %v:%v\n", conf.Pg.Server, uint16(conf.Pg.Port))

	// Connect to memcached server
	memCache = memcache.New(conf.Cache.Server)

	// Test the memcached connection
	cacheTest := memcache.Item{Key: "connecttext", Value: []byte("1"), Expiration: 10}
	err = memCache.Set(&cacheTest)
	if err != nil {
		log.Fatalf("Memcached server seems offline: %s", err)
	}

	// Log successful connection message for Memcached
	log.Printf("Connected to Memcached: %v\n", conf.Cache.Server)

	// Our pages
	http.HandleFunc("/", logReq(mainHandler))
	http.HandleFunc("/login", logReq(loginHandler))
	http.HandleFunc("/logout", logReq(logoutHandler))
	http.HandleFunc("/pref", logReq(prefHandler))
	http.HandleFunc("/register", logReq(registerHandler))
	http.HandleFunc("/stars/", logReq(starsHandler))
	http.HandleFunc("/upload/", logReq(uploadFormHandler))
	http.HandleFunc("/vis/", logReq(visualisePage))
	http.HandleFunc("/x/download/", logReq(downloadHandler))
	http.HandleFunc("/x/downloadcsv/", logReq(downloadCSVHandler))
	http.HandleFunc("/x/star/", logReq(starToggleHandler))
	http.HandleFunc("/x/table/", logReq(tableViewHandler))
	http.HandleFunc("/x/uploaddata/", logReq(uploadDataHandler))
	http.HandleFunc("/x/visdata/", logReq(visData))

	// Static files
	http.HandleFunc("/images/auth0.svg", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "images/auth0.svg")
	}))
	http.HandleFunc("/images/rackspace.svg", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "images/rackspace.svg")
	}))
	http.HandleFunc("/images/sqlitebrowser.svg", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "images/sqlitebrowser.svg")
	}))
	http.HandleFunc("/favicon.ico", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "favicon.ico")
	}))
	http.HandleFunc("/robots.txt", logReq(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "robots.txt")
	}))

	// Start server
	log.Printf("DBHub server starting on https://%s\n", conf.Web.Server)
	log.Fatal(http.ListenAndServeTLS(conf.Web.Server, conf.Web.Certificate, conf.Web.CertificateKey, nil))
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Main handler"

	// Split the request URL into path components
	pathStrings := strings.Split(r.URL.Path, "/")

	// numPieces will be 2 if the request was for the root directory (https://server/), or if
	// the request included only a single path component (https://server/someuser/)
	numPieces := len(pathStrings)
	if numPieces == 2 {
		userName := pathStrings[1]
		// Check if the request was for the root directory
		if pathStrings[1] == "" {
			// Yep, root directory request
			frontPage(w, r)
			return
		}

		// The request was for a user page
		userPage(w, r, userName)
		return
	}

	userName := pathStrings[1]
	dbName := pathStrings[2]

	// Validate the user supplied user and database name
	err := com.ValidateUserDB(userName, dbName)
	if err != nil {
		log.Printf("%s: Validation failed of user or database name. Username: '%v', Database: '%s', Error: %s",
			pageName, userName, dbName, err)
		errorPage(w, r, http.StatusBadRequest, "Invalid user or database name")
		return
	}

	// This catches the case where a "/" is on the end of a user page URL
	// TODO: Refactor this and the above identical code.  Doing it this way is non-optimal
	if pathStrings[2] == "" {
		// The request was for a user page
		userPage(w, r, userName)
		return
	}

	// * A specific database was requested *

	// Check if a table name was also requested
	err = r.ParseForm()
	if err != nil {
		log.Printf("%s: Error with ParseForm() in main handler: %s\n", pageName, err)
	}
	dbTable := r.FormValue("table")

	// If a table name was supplied, validate it
	if dbTable != "" {
		err = com.ValidatePGTable(dbTable)
		if err != nil {
			// Validation failed, so don't pass on the table name
			log.Printf("%s: Validation failed for table name: %s", pageName, err)
			dbTable = ""
		}
	}

	// TODO: Add support for folders and sub-folders in request paths
	databasePage(w, r, userName, dbName, dbTable)
}

func registerHandler(w http.ResponseWriter, r *http.Request) {
	// TODO: Add browser side validation of the form data too (using AngularJS?) to save a trip to the server
	// TODO  and make for a nicer user experience for sign up

	// Gather submitted form data (if any)
	err := r.ParseForm()
	if err != nil {
		log.Printf("Error when parsing registration data: %s\n", err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing registration data")
		return
	}
	userName := r.PostFormValue("username")
	password := r.PostFormValue("pass")
	passConfirm := r.PostFormValue("pconfirm")
	email := r.PostFormValue("email")
	agree := r.PostFormValue("agree")

	// Check if any (relevant) form data was submitted
	if userName == "" && password == "" && passConfirm == "" && email == "" && agree == "" {
		// No, so render the registration page
		registerPage(w, r)
		return
	}

	// Validate the user supplied username and email address
	err = com.ValidateUserEmail(userName, email)
	if err != nil {
		log.Printf("Validation failed of username or email: %s", err)
		errorPage(w, r, http.StatusBadRequest, "Invalid username or email")
		return
	}

	// Check the password and confirmation match
	if len(password) != len(passConfirm) || password != passConfirm {
		log.Println("Password and confirmation do not match")
		errorPage(w, r, http.StatusBadRequest, "Password and confirmation do not match")
		return
	}

	// Check the password isn't blank
	if len(password) < 6 {
		log.Println("Password must be 6 characters or greater")
		errorPage(w, r, http.StatusBadRequest, "Password must be 6 characters or greater")
		return
	}

	// Check the Terms and Conditions was agreed to
	if agree != "on" {
		log.Println("Terms and Conditions wasn't agreed to")
		errorPage(w, r, http.StatusBadRequest, "Terms and Conditions weren't agreed to")
		return
	}

	// Ensure the username isn't a reserved one
	err = com.ReservedUsernamesCheck(userName)
	if err != nil {
		log.Println(err)
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Check if the username is already in our system
	exists, err := com.CheckUserExists(userName)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Username check failed")
		return
	}
	if exists {
		errorPage(w, r, http.StatusConflict, "That username is already taken")
		return
	}

	// Check if the email address is already in our system
	exists, err = com.CheckEmailExists(email)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Email check failed")
		return
	}
	if exists {
		errorPage(w, r, http.StatusConflict,
			"That email address is already associated with an account in our system")
		return
	}

	// Add the user to the system
	err = com.AddUser(userName, password, email)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Something went wrong during user creation")
		return
	}

	// TODO: Display a proper success page
	// TODO: This should probably bounce the user to their logged in profile page
	w.WriteHeader(http.StatusCreated)
	fmt.Fprint(w, `<html><body>Account created successfully, please login: <a href="/login">Login</a></body></html>`)
}

// This handles incoming requests for the preferences page by logged in users
func prefHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Preferences handler"

	// Ensure user is logged in
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
	} else {
		// Bounce to the login page
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}

	// Gather submitted form data (if any)
	err := r.ParseForm()
	if err != nil {
		log.Printf("%s: Error when parsing preference data: %s\n", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing preference data")
		return
	}
	maxRows := r.PostFormValue("maxrows")

	// If no form data was submitted, display the preferences page form
	if maxRows == "" {
		prefPage(w, r, fmt.Sprintf("%s", loggedInUser))
		return
	}

	// Validate submitted form data
	err = com.Validate.Var(maxRows, "required,numeric,min=1,max=500")
	if err != nil {
		log.Printf("%s: Preference data failed validation: %s\n", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Error when parsing preference data")
		return
	}

	// Update the preference data in the database
	err = com.SetPrefUserMaxRows(loggedInUser, maxRows)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, "Error when updating preferences")
		return
	}

	// Bounce to the user home page
	http.Redirect(w, r, "/"+loggedInUser, http.StatusTemporaryRedirect)
}

// Handles JSON requests from the front end to toggle a database's star
func starToggleHandler(w http.ResponseWriter, r *http.Request) {
	// Extract the user and database name
	dbOwner, dbName, err := com.GetOD(2, r) // 2 = Ignore "/x/star/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Retrieve session data (if any)
	var loggedInUser interface{}
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = sess.CAttr("UserName")
	} else {
		// No logged in username, so nothing to update
		fmt.Fprint(w, "-1") // -1 tells the front end not to update the displayed star count
		return
	}

	// Toggle on or off the starring of a database by a user
	err = com.ToggleDBStar(loggedInUser, dbOwner, dbName)
	if err != nil {
		fmt.Fprint(w, "-1") // -1 tells the front end not to update the displayed star count
		return
	}

	// Return the updated star count
	newStarCount, err := com.DBStars(dbOwner, dbName)
	if err != nil {
		fmt.Fprint(w, "-1") // -1 tells the front end not to update the displayed star count
		return
	}
	fmt.Fprint(w, newStarCount)
}

func starsHandler(w http.ResponseWriter, r *http.Request) {
	// Retrieve user and database name
	dbOwner, dbName, err := com.GetOD(1, r) // 2 = Ignore "/stars/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Render the stars page
	starsPage(w, r, dbOwner, dbName)
}

// This passes table row data back to the main UI in JSON format
func tableViewHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Table data handler"

	// TODO: Add support for database versions too

	// Retrieve user, database, and table name
	dbOwner, dbName, requestedTable, err := com.GetODT(2, r) // 1 = Ignore "/x/table/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
	}

	// Check if the user has access to the requested database
	var dbQuery, jsonCacheKey, queryCacheKey string
	if loggedInUser != dbOwner {
		// * The request is for another users database, so it needs to be a public one *
		dbQuery = `
			WITH requested_db AS (
				SELECT idnum, minio_bucket
				FROM sqlite_databases
				WHERE username = $1
					AND dbname = $2
			)
			SELECT db.minio_bucket, ver.minioid
			FROM database_versions AS ver, requested_db AS db
			WHERE ver.db = db.idnum
				AND ver.public = true
			ORDER BY version DESC
			LIMIT 1`
		tempArr := md5.Sum([]byte(dbOwner + "/" + dbName + "/" + requestedTable))
		jsonCacheKey = "tbl-pub-" + hex.EncodeToString(tempArr[:])
		tempArr2 := md5.Sum([]byte(fmt.Sprintf(dbQuery, dbOwner, dbName)))
		queryCacheKey = "pub/" + hex.EncodeToString(tempArr2[:])

	} else {
		dbQuery = `
			WITH requested_db AS (
				SELECT idnum, minio_bucket
				FROM sqlite_databases
				WHERE username = $1
					AND dbname = $2
			)
			SELECT db.minio_bucket, ver.minioid
			FROM database_versions AS ver, requested_db AS db
			WHERE ver.db = db.idnum
			ORDER BY version DESC
			LIMIT 1`
		tempArr := md5.Sum([]byte(loggedInUser + "-" + dbOwner + "/" + dbName + "/" + requestedTable))
		jsonCacheKey = "tbl-" + hex.EncodeToString(tempArr[:])
		tempArr2 := md5.Sum([]byte(fmt.Sprintf(dbQuery, dbOwner, dbName)))
		queryCacheKey = loggedInUser + "/" + hex.EncodeToString(tempArr2[:])
	}

	var jsonResponse []byte
	var minioInfo struct {
		Bucket string
		Id     string
	}

	// Use a cached version of the query response if it exists
	ok, err := getCachedData(queryCacheKey, &minioInfo)
	if err != nil {
		log.Printf("%s: Error retrieving data from cache: %v\n", pageName, err)
	}
	if !ok {
		// Cached version doesn't exist, so query the database
		err = db.QueryRow(dbQuery, dbOwner, dbName).Scan(&minioInfo.Bucket, &minioInfo.Id)
		if err != nil {
			log.Printf("%s: Error looking up MinioID. Owner: '%s' Database: %v Error: %v\n", pageName,
				dbOwner, dbName, err)
			return
		}

		// Cache the database details
		err = cacheData(queryCacheKey, minioInfo, 120)
		if err != nil {
			log.Printf("%s: Error when caching page data: %v\n", pageName, err)
		}
	}

	// Sanity check
	if minioInfo.Id == "" {
		// The requested database wasn't found
		log.Printf("%s: Requested database not found. Owner: '%s' Database: '%s'", pageName, dbOwner, dbName)
		return
	}

	// Determine the number of rows to display
	var maxRows int
	if loggedInUser != "" {
		// Retrieve the user preference data
		maxRows = getUserMaxRowsPref(loggedInUser)
	} else {
		// Not logged in, so default to 10 rows
		maxRows = 10
	}

	// Use a cached version of the full json response if it exists
	jsonCacheKey += "/" + strconv.Itoa(maxRows)
	ok, err = getCachedData(jsonCacheKey, &jsonResponse)
	if err != nil {
		log.Printf("%s: Error retrieving data from cache: %v\n", pageName, err)
	}
	if ok {
		// Serve the response from cache
		fmt.Fprintf(w, "%s", jsonResponse)
		return
	}

	// Get a handle from Minio for the database object
	userDB, err := minioClient.GetObject(minioInfo.Bucket, minioInfo.Id)
	if err != nil {
		log.Printf("%s: Error retrieving DB from Minio: %v\n", pageName, err)
		return
	}

	// Close the object handle when this function finishes
	defer func() {
		err := userDB.Close()
		if err != nil {
			log.Printf("%s: Error closing object handle: %v\n", pageName, err)
		}
	}()

	// Save the database locally to a temporary file
	tempfileHandle, err := ioutil.TempFile("", "databaseViewHandler-")
	if err != nil {
		log.Printf("%s: Error creating tempfile: %v\n", pageName, err)
		return
	}
	tempfile := tempfileHandle.Name()
	bytesWritten, err := io.Copy(tempfileHandle, userDB)
	if err != nil {
		log.Printf("%s: Error writing database to temporary file: %v\n", pageName, err)
		return
	}
	if bytesWritten == 0 {
		log.Printf("%s: 0 bytes written to the temporary file: %v\n", pageName, dbName)
		return
	}
	tempfileHandle.Close()
	defer os.Remove(tempfile) // Delete the temporary file when this function finishes

	// Open database
	db, err := sqlite.Open(tempfile, sqlite.OpenReadOnly)
	if err != nil {
		log.Printf("Couldn't open database: %s", err)
		return
	}
	defer db.Close()

	// Retrieve the list of tables in the database
	tables, err := db.Tables("")
	if err != nil {
		log.Printf("Error retrieving table names: %s", err)
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("The database '%s' doesn't seem to have any tables. Aborting.", dbName)
		return
	}

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

	// If no specific table was requested, use the first one
	if requestedTable == "" {
		requestedTable = tables[0]
	}

	// Read the data from the database
	dataRows, err := readSQLiteDB(db, requestedTable, maxRows)
	if err != nil {
		// Some kind of error when reading the database data
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Count the total number of rows in the requested table
	dataRows.TotalRows, err = getSQLiteRowCount(db, requestedTable)
	if err != nil {
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Format the output
	if dataRows.RowCount > 0 {
		// Use json.MarshalIndent() for nicer looking output
		jsonResponse, err = json.MarshalIndent(dataRows, "", " ")
		if err != nil {
			log.Println(err)
			return
		}
	} else {
		// Return an empty set indicator, instead of "null"
		jsonResponse = []byte{'{', ']'}
	}

	// Cache the JSON data
	err = cacheData(jsonCacheKey, jsonResponse, cacheTime)
	if err != nil {
		log.Printf("%s: Error when caching JSON data: %v\n", pageName, err)
	}

	//w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, "%s", jsonResponse)
}

// This function presents the database upload form to logged in users
func uploadFormHandler(w http.ResponseWriter, r *http.Request) {
	// Ensure user is logged in
	var loggedInUser interface{}
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = sess.CAttr("UserName")
	} else {
		errorPage(w, r, http.StatusUnauthorized, "You need to be logged in")
		return
	}

	// Render the upload page
	uploadPage(w, r, fmt.Sprintf("%s", loggedInUser))
}

// This function processes new database data submitted through the upload form
func uploadDataHandler(w http.ResponseWriter, r *http.Request) {
	pageName := "Upload DB handler"

	// Ensure user is logged in
	var loggedInUser string
	sess := session.Get(r)
	if sess == nil {
		errorPage(w, r, http.StatusUnauthorized, "You need to be logged in")
		return
	}
	loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))

	// Prepare the form data
	r.ParseMultipartForm(32 << 20) // 64MB of ram max
	if err := r.ParseForm(); err != nil {
		log.Printf("%s: ParseForm() error: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, err.Error())
		return
	}

	// Grab and validate the supplied "public" form field
	userPublic := r.PostFormValue("public")
	public, err := strconv.ParseBool(userPublic)
	if err != nil {
		log.Printf("%s: Error when converting public value to boolean: %v\n", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Public value incorrect")
		return
	}

	// TODO: Add support for folders and subfolders
	folder := "/"

	tempFile, handler, err := r.FormFile("database")
	if err != nil {
		log.Printf("%s: Uploading file failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database file missing from upload data?")
		return
	}
	dbName := handler.Filename
	defer tempFile.Close()

	// Validate the database name
	err = com.ValidateDB(dbName)
	if err != nil {
		log.Printf("%s: Validation failed for database name: %s", pageName, err)
		errorPage(w, r, http.StatusBadRequest, "Invalid database name")
		return
	}

	// Write the temporary file locally, so we can try opening it with SQLite to verify it's ok
	var tempBuf bytes.Buffer
	bytesWritten, err := io.Copy(&tempBuf, tempFile)
	if err != nil {
		log.Printf("%s: Error: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	if bytesWritten == 0 {
		log.Printf("%s: Database seems to be 0 bytes in length. Username: %s, Database: %s\n", pageName,
			loggedInUser, dbName)
		errorPage(w, r, http.StatusBadRequest, "Database file is 0 length?")
		return
	}
	tempDB, err := ioutil.TempFile("", "dbhub-upload-")
	if err != nil {
		log.Printf("%s: Error creating temporary file. User: %s, Database: %s, Filename: %s, Error: %v\n",
			pageName, loggedInUser, dbName, tempDB.Name(), err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	_, err = tempDB.Write(tempBuf.Bytes())
	if err != nil {
		log.Printf("%s: Error when writing the uploaded db to a temp file. User: %s, Database: %s"+
			"Error: %v\n", pageName, loggedInUser, dbName, err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	tempDBName := tempDB.Name()

	// Delete the temporary file when this function finishes
	defer os.Remove(tempDBName)

	// Perform a read on the database, as a basic sanity check to ensure it's really a SQLite database
	sqliteDB, err := sqlite.Open(tempDBName, sqlite.OpenReadOnly)
	if err != nil {
		log.Printf("Couldn't open database when sanity checking upload: %s", err)
		errorPage(w, r, http.StatusInternalServerError, "Internal error")
		return
	}
	defer sqliteDB.Close()
	tables, err := sqliteDB.Tables("")
	if err != nil {
		log.Printf("Error retrieving table names when sanity checking upload: %s", err)
		errorPage(w, r, http.StatusInternalServerError,
			"Error when sanity checking file.  Possibly encrypted or not a database?")
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("The attemped upload for '%s' failed, as it doesn't seem to have any tables.", dbName)
		errorPage(w, r, http.StatusInternalServerError, "Database has no tables?")
		return
	}

	// Generate sha256 of the uploaded file
	shaSum := sha256.Sum256(tempBuf.Bytes())

	// Determine the version number for this new database
	highVer, err := com.HighestDBVersion(loggedInUser, dbName)
	var newVer int
	if highVer > 0 {
		// The database already exists
		newVer = highVer + 1
	} else {
		newVer = 1
	}

	// Retrieve the Minio bucket to store the database in
	var minioBucket string
	err = db.QueryRow(`
		SELECT minio_bucket
		FROM users
		WHERE username = $1`, loggedInUser).Scan(&minioBucket)
	if err != nil && err != pgx.ErrNoRows {
		log.Printf("%s: Error when querying database: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failure")
		return
	}

	// Generate random filename to store the database as
	mathrand.Seed(time.Now().UnixNano())
	const alphaNum = "abcdefghijklmnopqrstuvwxyz0123456789"
	randomString := make([]byte, 8)
	for i := range randomString {
		randomString[i] = alphaNum[mathrand.Intn(len(alphaNum))]
	}
	minioId := string(randomString) + ".db"

	// TODO: We should probably check if the randomly generated filename is already used for the user, just in case

	// Store the database file in Minio
	dbSize, err := minioClient.PutObject(minioBucket, minioId, &tempBuf, handler.Header["Content-Type"][0])
	if err != nil {
		log.Printf("%s: Storing file in Minio failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Storing in object store failed")
		return
	}

	// TODO: Put these queries inside a single transaction

	// Add the new database details to the PG database
	var dbQuery string
	if newVersion == 1 {
		dbQuery = `
			INSERT INTO sqlite_databases (username, folder, dbname, minio_bucket)
			VALUES ($1, $2, $3, $4)`
		commandTag, err := db.Exec(dbQuery, loggedInUser, folder, dbName, minioBucket)
		if err != nil {
			log.Printf("%s: Adding database to PostgreSQL failed: %v\n", pageName, err)
			errorPage(w, r, http.StatusInternalServerError, "Database query failed")
			return
		}
		if numRows := commandTag.RowsAffected(); numRows != 1 {
			log.Printf("%s: Wrong number of rows affected: %v, user: %s, database: %v\n", pageName,
				numRows, loggedInUser, dbName)
			return
		}
	}

	// Add the database to database_versions
	dbQuery = `
		WITH databaseid AS (
			SELECT idnum
			FROM sqlite_databases
			WHERE username = $1
				AND dbname = $2)
		INSERT INTO database_versions (db, size, version, sha256, public, minioid)
		SELECT idnum, $3, $4, $5, $6, $7 FROM databaseid`
	commandTag, err := db.Exec(dbQuery, loggedInUser, dbName, dbSize, newVersion, hex.EncodeToString(shaSum[:]),
		public, minioId)
	if err != nil {
		log.Printf("%s: Adding version info to PostgreSQL failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}

	// Update the last_modified date for the database in sqlite_databases
	dbQuery = `
		UPDATE sqlite_databases
		SET last_modified = (
			SELECT last_modified
			FROM database_versions
			WHERE db = (
				SELECT idnum
				FROM sqlite_databases
				WHERE username = $1
					AND dbname = $2)
				AND version = $3)
		WHERE username = $1
			AND dbname = $2`
	commandTag, err = db.Exec(dbQuery, loggedInUser, dbName, newVersion)
	if err != nil {
		log.Printf("%s: Updating last_modified date in PostgreSQL failed: %v\n", pageName, err)
		errorPage(w, r, http.StatusInternalServerError, "Database query failed")
		return
	}
	if numRows := commandTag.RowsAffected(); numRows != 1 {
		log.Printf("%s: Wrong number of rows affected: %v, user: %s, database: %v\n", pageName, numRows,
			loggedInUser, dbName)
		return
	}

	// Log the successful database upload
	log.Printf("%s: Username: %v, database '%v' uploaded as '%v', bytes: %v\n", pageName, loggedInUser, dbName,
		minioId, dbSize)

	// Database upload succeeded.  Tell the user then bounce back to their profile page
	fmt.Fprintf(w, `
	<html><head><script type="text/javascript"><!--
		function delayer(){
			window.location = "/%s"
		}//-->
	</script></head>
	<body onLoad="setTimeout('delayer()', 5000)">
	<body>Upload succeeded<br /><br /><a href="/%s">Continuing to profile page...</a></body></html>`,
		loggedInUser, loggedInUser)
}

// Receives a request for specific table data from the front end, returning it as JSON
func visData(w http.ResponseWriter, r *http.Request) {
	pageName := "Visualisation data handler"

	var pageData struct {
		Meta com.MetaInfo
		DB   com.SqliteDBinfo
		Data com.SqliteRecordSet
	}

	// Retrieve user, database, and table name
	userName, dbName, requestedTable, err := com.GetODT(2, r) // 1 = Ignore "/x/table/" at the start of the URL
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Check if X and Y column names were given
	var reqXCol, reqYCol, xCol, yCol string
	reqXCol = r.FormValue("xcol")
	reqYCol = r.FormValue("ycol")

	// Validate column names if present
	// FIXME: Create a proper validation function for SQLite column names
	if reqXCol != "" {
		err = com.ValidatePGTable(reqXCol)
		if err != nil {
			log.Printf("Validation failed for SQLite column name: %s", err)
			return
		}
		xCol = reqXCol
	}
	if reqYCol != "" {
		err = com.ValidatePGTable(reqYCol)
		if err != nil {
			log.Printf("Validation failed for SQLite column name: %s", err)
			return
		}
		yCol = reqYCol
	}

	// Validate WHERE clause values if present
	var reqWCol, reqWType, reqWVal, wCol, wType, wVal string
	reqWCol = r.FormValue("wherecol")
	reqWType = r.FormValue("wheretype")
	reqWVal = r.FormValue("whereval")

	// WHERE column
	if reqWCol != "" {
		err = com.ValidatePGTable(reqWCol)
		if err != nil {
			log.Printf("Validation failed for SQLite column name: %s", err)
			return
		}
		wCol = reqWCol
	}

	// WHERE type
	switch reqWType {
	case "":
		// We don't pass along empty values
	case "LIKE", "=", "!=", "<", "<=", ">", ">=":
		wType = reqWType
	default:
		// This should never be reached
		log.Printf("%s: Validation failed on WHERE clause type. wType = '%v'\n", pageName, wType)
		return
	}

	// TODO: Add ORDER BY clause
	// TODO: We'll probably need some kind of optional data transformation for columns too
	// TODO    eg column foo → DATE (type)

	// WHERE value
	var whereClauses []com.WhereClause
	if reqWVal != "" && wType != "" {
		whereClauses = append(whereClauses, com.WhereClause{Column: wCol, Type: wType, Value: reqWVal})

		// TODO: Double check if we should be filtering out potentially devious characters here. I don't
		// TODO  (at the moment) *think* we need to, as we're using parameter binding on the passed in values
		wVal = reqWVal
	}

	// Retrieve session data (if any)
	var loggedInUser string
	sess := session.Get(r)
	if sess != nil {
		loggedInUser = fmt.Sprintf("%s", sess.CAttr("UserName"))
	}

	// Check if the user has access to the requested database
	err = com.CheckUserDBAccess(&pageData.DB, loggedInUser, userName, dbName)
	if err != nil {
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// * Execution can only get here if the user has access to the requested database *

	// Generate a predictable cache key for the JSON data
	var pageCacheKey string
	if loggedInUser != userName {
		tempArr := md5.Sum([]byte(userName + "/" + dbName + "/" + requestedTable + xCol + yCol + wCol +
			wType + wVal))
		pageCacheKey = "visdat-pub-" + hex.EncodeToString(tempArr[:])
	} else {
		tempArr := md5.Sum([]byte(loggedInUser + "-" + userName + "/" + dbName + "/" + requestedTable +
			xCol + yCol + wCol + wType + wVal))
		pageCacheKey = "visdat-" + hex.EncodeToString(tempArr[:])
	}

	// If a cached version of the page data exists, use it
	var jsonResponse []byte
	ok, err := getCachedData(pageCacheKey, &jsonResponse)
	if err != nil {
		log.Printf("%s: Error retrieving page data from cache: %v\n", pageName, err)
	}
	if ok {
		// Render the JSON response from cache
		fmt.Fprintf(w, "%s", jsonResponse)
		return
	}

	// Get a handle from Minio for the database object
	sdb, err := com.OpenMinioObject(pageData.DB.MinioBkt, pageData.DB.MinioId)
	if err != nil {
		return
	}

	// Retrieve the list of tables in the database
	tables, err := sdb.Tables("")
	if err != nil {
		log.Printf("%s: Error retrieving table names: %s", pageName, err)
		return
	}
	if len(tables) == 0 {
		// No table names were returned, so abort
		log.Printf("%s: The database '%s' doesn't seem to have any tables. Aborting.", pageName, dbName)
		return
	}
	pageData.DB.Info.Tables = tables

	// If a specific table was requested, check that it's present
	var dbTable string
	if requestedTable != "" {
		// Check the requested table is present
		for _, tbl := range tables {
			if tbl == requestedTable {
				dbTable = requestedTable
			}
		}
	}

	// If a specific table wasn't requested, use the first table in the database
	if dbTable == "" {
		dbTable = pageData.DB.Info.Tables[0]
	}

	// Retrieve the table data requested by the user
	maxVals := 2500 // 2500 row maximum for now
	if xCol != "" && yCol != "" {
		pageData.Data, err = com.ReadSQLiteDBCols(sdb, requestedTable, true, true, maxVals, whereClauses, xCol, yCol)
	} else {
		pageData.Data, err = com.ReadSQLiteDB(sdb, requestedTable, maxVals)
	}
	if err != nil {
		// Some kind of error when reading the database data
		errorPage(w, r, http.StatusBadRequest, err.Error())
		return
	}

	// Use json.MarshalIndent() for nicer looking output
	jsonResponse, err = json.Marshal(pageData.Data)
	if err != nil {
		log.Println(err)
		return
	}

	// Cache the JSON data
	err = com.CacheData(pageCacheKey, jsonResponse, com.CacheTime)
	if err != nil {
		log.Printf("%s: Error when caching JSON data: %v\n", pageName, err)
	}

	//w.Header().Set("Access-Control-Allow-Origin", "*")
	fmt.Fprintf(w, "%s", jsonResponse)
}
