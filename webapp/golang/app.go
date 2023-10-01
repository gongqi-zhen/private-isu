package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/sha512"
	"database/sql"
	"encoding/hex"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	txtemplate "text/template"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
	cmap "github.com/orcaman/concurrent-map/v2"
)

var (
	db    *sqlx.DB
	store *gsm.MemcacheStore
	sf    = singleflight.Group{}
)

const (
	postsPerPage  = 20
	ISO8601Format = "2006-01-02T15:04:05-07:00"
	UploadLimit   = 10 * 1024 * 1024 // 10mb
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int       `db:"comment_count"`
	RN           int       `db:"rn"`
	Comment      NullComment
	Comments     []Comment
	User         User
	CSRFToken    string
}

type NullComment struct {
	ID        sql.NullInt64  `db:"id"`
	PostID    sql.NullInt64  `db:"post_id"`
	UserID    sql.NullInt64  `db:"user_id"`
	Comment   sql.NullString `db:"comment"`
	CreatedAt sql.NullTime   `db:"created_at"`
	User      NullUser
}

type NullUser struct {
	ID          sql.NullInt64  `db:"id"`
	AccountName sql.NullString `db:"account_name"`
	Authority   sql.NullInt64  `db:"authority"`
	DelFlg      sql.NullInt64  `db:"del_flg"`
	CreatedAt   sql.NullTime   `db:"created_at"`
}

type Comment struct {
	Comment    string
	AuthorName string
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient := memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}
}

func deleteImageFiles() {
	files, err := os.ReadDir("/home/public/image")
	if err != nil {
		fmt.Println("Error reading directory:", err)
		return
	}

	for _, file := range files {
		fileName := file.Name()
		parts := strings.Split(fileName, ".")
		if len(parts) != 2 {
			continue
		}

		idx, err := strconv.Atoi(parts[0])
		if err != nil {
			fmt.Println("Error converting string to integer:", err)
			continue
		}

		if idx > 10000 {
			err := os.Remove("/home/public/image/" + fileName)
			if err != nil {
				fmt.Println("Error deleting file:", err)
			} else {
				fmt.Println("Deleted:", fileName)
			}
		}
	}
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

func digest(src string) string {
	hasher := sha512.New()
	hasher.Write([]byte(src))
	hash := hasher.Sum(nil)
	return hex.EncodeToString(hash)
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}

	if u, ok := userCache.Get(strconv.Itoa(uid.(int))); ok {
		return u
	}

	err := db.Get(&u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	if err != nil {
		return User{}
	}
	userCache.Set(strconv.Itoa(u.ID), u)

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func makePosts(results []Post, csrfToken string) ([]Post, error) {
	postMap := make(map[int]Post, postsPerPage)

	for _, p := range results {
		if _, ok := postMap[p.ID]; !ok {
			postMap[p.ID] = p
		}

		if p.Comment.ID.Valid {
			p.Comments = append(p.Comments, Comment{
				Comment:    p.Comment.Comment.String,
				AuthorName: p.Comment.User.AccountName.String,
			})
		}
	}

	posts := make([]Post, 0, len(postMap))
	for _, p := range postMap {
		p.CSRFToken = csrfToken
		posts = append(posts, p)
	}

	slices.SortFunc(posts, func(i Post, j Post) int {
		// 降順
		return int(j.CreatedAt.UnixNano() - i.CreatedAt.UnixNano())
	})

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

var (
	userCache = cmap.New[User]()
)

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	deleteImageFiles()
	userCache.Clear()
	w.WriteHeader(http.StatusOK)
}

var (
	loginTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html"),
	))
)

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	loginTemplate.Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		userCache.Set(strconv.Itoa(u.ID), *u)
		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "アカウント名かパスワードが間違っています"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

var (
	registerTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html"),
	))
)

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	registerTemplate.Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "アカウント名は3文字以上、パスワードは6文字以上である必要があります"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ユーザーが存在しない場合はエラーになるのでエラーチェックはしない
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "アカウント名がすでに使われています"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.Exec(query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = int(uid)
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

var (
	indexTemplate = txtemplate.Must(txtemplate.New("layout.html").Funcs(txtemplate.FuncMap{
		"imageURL": imageURL,
		"escape":   txtemplate.HTMLEscapeString,
	}).ParseFiles(
		getTemplPath("index/layout.html"),
	))

	indexContentTemplate = txtemplate.Must(txtemplate.New("index.html").Funcs(txtemplate.FuncMap{
		"imageURL": imageURL,
		"escape":   txtemplate.HTMLEscapeString,
	}).ParseFiles(
		getTemplPath("index/index.html"),
		getTemplPath("index/posts.html"),
		getTemplPath("index/post.html"),
	))
)

var (
	indexContent    = ""
	indexPostsMutex = sync.RWMutex{}
	lastUpdated     = time.Now()
	lastTriggered   = time.Now()
)

func updateIndexPosts() (string, error) {
	indexPostsMutex.RLock()
	if lastUpdated.After(lastTriggered) {
		defer indexPostsMutex.RUnlock()
		return indexContent, nil
	}
	indexPostsMutex.RUnlock()

	_, err, _ := sf.Do("indexPosts", func() (interface{}, error) {
		indexPostsMutex.Lock()
		lastTriggered = time.Now()
		defer indexPostsMutex.Unlock()
		results := []Post{}
		err := db.Select(&results, "WITH pu AS ( SELECT "+
			"`posts`.`id`, `posts`.`user_id`, `posts`.`body`, `posts`.`mime`, `posts`.`created_at`, "+
			"`users`.`id` AS `user.id`, `users`.`account_name` AS `user.account_name`, `users`.`authority` AS `user.authority`, `users`.`del_flg` AS `user.del_flg`, `users`.`created_at` AS `user.created_at` "+
			"FROM `posts` LEFT JOIN `users` ON `users`.`id` = `posts`.`user_id` WHERE `users`.`del_flg` = 0 ORDER BY `posts`.`created_at` DESC LIMIT ? ), "+
			"pc AS ( SELECT "+
			"pu.*, "+
			"`comments`.`id` AS `comment.id`, `comments`.`user_id` AS `comment.user_id`, `comments`.`comment` AS `comment.comment`, `comments`.`created_at` AS `comment.created_at`, "+
			"`users`.`id` AS `comment.user.id`, `users`.`account_name` AS `comment.user.account_name`, `users`.`authority` AS `comment.user.authority`, `users`.`del_flg` AS `comment.user.del_flg`, `users`.`created_at` AS `comment.user.created_at`, "+
			"ROW_NUMBER() OVER (PARTITION BY pu.id ORDER BY comments.created_at) AS rn, COUNT(*) OVER (PARTITION BY pu.id) AS comment_count "+
			"FROM pu "+
			"LEFT JOIN `comments` ON `comments`.`post_id` = `pu`.`id` "+
			"LEFT JOIN `users` ON `users`.`id` = `comments`.`user_id` "+
			"ORDER BY `comments`.`created_at` ) "+
			"SELECT * FROM pc WHERE (rn <= 3 OR rn IS NULL)", postsPerPage)
		if err != nil {
			log.Print(err)
			return nil, err
		}
		posts, err := makePosts(results, "")
		if err != nil {
			return nil, err
		}

		indexContentBuf := bytes.NewBuffer(nil)
		indexContentTemplate.ExecuteTemplate(indexContentBuf, "index.html", struct {
			Posts []Post
		}{posts})

		indexContent = indexContentBuf.String()
		lastUpdated = time.Now()
		return nil, nil
	})

	indexPostsMutex.RLock()
	defer indexPostsMutex.RUnlock()

	if err != nil {
		return "", err
	}
	return indexContent, nil
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	indexContent, err := updateIndexPosts()
	if err != nil {
		log.Print(err)
		return
	}

	indexContent = strings.Replace(indexContent, "<<CSRFToken>>", getCSRFToken(r), -1)
	if flash := getFlash(w, r, "notice"); flash != "" {
		indexContent = strings.Replace(indexContent, "##Flash##", `<div id="notice-message" class="alert alert-danger">`+flash+`</div>`, -1)
	} else {
		indexContent = strings.Replace(indexContent, "##Flash##", "", -1)
	}

	indexTemplate.Execute(w, struct {
		Me      User
		Content string
	}{me, indexContent})
}

var (
	userTemplate = template.Must(template.New("layout.html").Funcs(template.FuncMap{
		"imageURL": imageURL,
	}).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
)

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := chi.URLParam(r, "accountName")
	user := User{}

	err := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	err = db.Select(&results, "WITH pu AS ( SELECT "+
		"`posts`.`id`, `posts`.`user_id`, `posts`.`body`, `posts`.`mime`, `posts`.`created_at`, "+
		"`users`.`id` AS `user.id`, `users`.`account_name` AS `user.account_name`, `users`.`authority` AS `user.authority`, `users`.`del_flg` AS `user.del_flg`, `users`.`created_at` AS `user.created_at` "+
		"FROM `posts` LEFT JOIN `users` ON `users`.`id` = `posts`.`user_id` WHERE `users`.`del_flg` = 0 AND `posts`.`user_id` = ? ORDER BY `posts`.`created_at` DESC LIMIT ? ), "+
		"pc AS ( SELECT "+
		"pu.*, "+
		"`comments`.`id` AS `comment.id`, `comments`.`user_id` AS `comment.user_id`, `comments`.`comment` AS `comment.comment`, `comments`.`created_at` AS `comment.created_at`, "+
		"`users`.`id` AS `comment.user.id`, `users`.`account_name` AS `comment.user.account_name`, `users`.`authority` AS `comment.user.authority`, `users`.`del_flg` AS `comment.user.del_flg`, `users`.`created_at` AS `comment.user.created_at`, "+
		"ROW_NUMBER() OVER (PARTITION BY pu.id ORDER BY comments.created_at) AS rn, COUNT(*) OVER (PARTITION BY pu.id) AS comment_count "+
		"FROM pu "+
		"LEFT JOIN `comments` ON `comments`.`post_id` = `pu`.`id` "+
		"LEFT JOIN `users` ON `users`.`id` = `comments`.`user_id` "+
		"ORDER BY `comments`.`created_at` ) "+
		"SELECT * FROM pc WHERE (rn <= 3 OR rn IS NULL)",
		user.ID, postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := []int{}
	err = db.Select(&postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []interface{}
		args := make([]interface{}, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	userTemplate.Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

var (
	postsTemplate = template.Must(template.New("posts.html").Funcs(template.FuncMap{
		"imageURL": imageURL,
	}).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	))
)

func getPosts(w http.ResponseWriter, r *http.Request) {
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	err = db.Select(&results, "WITH pu AS ( SELECT "+
		"`posts`.`id`, `posts`.`user_id`, `posts`.`body`, `posts`.`mime`, `posts`.`created_at`, "+
		"`users`.`id` AS `user.id`, `users`.`account_name` AS `user.account_name`, `users`.`authority` AS `user.authority`, `users`.`del_flg` AS `user.del_flg`, `users`.`created_at` AS `user.created_at` "+
		"FROM `posts` LEFT JOIN `users` ON `users`.`id` = `posts`.`user_id` WHERE `users`.`del_flg` = 0 AND `posts`.`created_at` <= ? ORDER BY `posts`.`created_at` DESC LIMIT ? ), "+
		"pc AS ( SELECT "+
		"pu.*, "+
		"`comments`.`id` AS `comment.id`, `comments`.`user_id` AS `comment.user_id`, `comments`.`comment` AS `comment.comment`, `comments`.`created_at` AS `comment.created_at`, "+
		"`users`.`id` AS `comment.user.id`, `users`.`account_name` AS `comment.user.account_name`, `users`.`authority` AS `comment.user.authority`, `users`.`del_flg` AS `comment.user.del_flg`, `users`.`created_at` AS `comment.user.created_at`, "+
		"ROW_NUMBER() OVER (PARTITION BY pu.id ORDER BY comments.created_at) AS rn, COUNT(*) OVER (PARTITION BY pu.id) AS comment_count "+
		"FROM pu "+
		"LEFT JOIN `comments` ON `comments`.`post_id` = `pu`.`id` "+
		"LEFT JOIN `users` ON `users`.`id` = `comments`.`user_id` "+
		"ORDER BY `comments`.`created_at` ) "+
		"SELECT * FROM pc WHERE (rn <= 3 OR rn IS NULL)",
		t.Format(ISO8601Format), postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	postsTemplate.Execute(w, posts)
}

var (
	postIDTemplate = template.Must(template.New("layout.html").Funcs(template.FuncMap{
		"imageURL": imageURL,
	}).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	))
)

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := chi.URLParam(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	err = db.Select(&results, "WITH pu AS ( SELECT "+
		"`posts`.`id`, `posts`.`user_id`, `posts`.`body`, `posts`.`mime`, `posts`.`created_at`, "+
		"`users`.`id` AS `user.id`, `users`.`account_name` AS `user.account_name`, `users`.`authority` AS `user.authority`, `users`.`del_flg` AS `user.del_flg`, `users`.`created_at` AS `user.created_at` "+
		"FROM `posts` LEFT JOIN `users` ON `users`.`id` = `posts`.`user_id` WHERE `users`.`del_flg` = 0 AND `posts`.`id` = ? LIMIT ? ), "+
		"pc AS ( SELECT "+
		"pu.*, "+
		"`comments`.`id` AS `comment.id`, `comments`.`user_id` AS `comment.user_id`, `comments`.`comment` AS `comment.comment`, `comments`.`created_at` AS `comment.created_at`, "+
		"`users`.`id` AS `comment.user.id`, `users`.`account_name` AS `comment.user.account_name`, `users`.`authority` AS `comment.user.authority`, `users`.`del_flg` AS `comment.user.del_flg`, `users`.`created_at` AS `comment.user.created_at`, "+
		"ROW_NUMBER() OVER (PARTITION BY pu.id ORDER BY comments.created_at) AS rn, COUNT(*) OVER (PARTITION BY pu.id) AS comment_count "+
		"FROM pu "+
		"LEFT JOIN `comments` ON `comments`.`post_id` = `pu`.`id` "+
		"LEFT JOIN `users` ON `users`.`id` = `comments`.`user_id` "+
		"ORDER BY `comments`.`created_at` ) "+
		"SELECT * FROM pc",
		pid, postsPerPage)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r))
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	postIDTemplate.Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "画像が必須です"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	if file != nil {
		// 投稿のContent-Typeからファイルのタイプを決定する
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "投稿できる画像形式はjpgとpngとgifだけです"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "ファイルサイズが大きすぎます"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.Exec(
		query,
		me.ID,
		mime,
		[]byte{},
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	filename := fmt.Sprintf("/home/public/image/%d.%s", pid, getExtension(mime))
	err = os.WriteFile(filename, filedata, 0644)
	if err != nil {
		log.Print("Could not write file: ", err)
		return
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getExtension(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	default:
		return ""
	}
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_idは整数のみです")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.Exec(query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

var (
	adminBannedTemplate = template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	)
)

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	adminBannedTemplate.Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
		userCache.Remove(id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local&interpolateParams=true",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(32)

	r := chi.NewRouter()

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[a-zA-Z]+}`, getAccountName)
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		http.FileServer(http.Dir("../public")).ServeHTTP(w, r)
	})

	// add pprof
	r.Mount("/debug", middleware.Profiler())
	log.Fatal(http.ListenAndServe(":8080", r))
}
