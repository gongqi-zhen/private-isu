package main

import (
	"fmt"
	"log"
	"os"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

type Post struct {
	ID      int    `db:"id"`
	Mime    string `db:"mime"`
	Imgdata []byte `db:"imgdata"`
}

func saveAllImages() {
	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local&interpolateParams=true",
		"root",
		"root",
		"127.0.0.1",
		"3306",
		"isuconp",
	)

	db, err := sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	offset := 0
	limit := 100

	for {
		posts := []Post{}
		err := db.Select(&posts, "SELECT `id`, `mime`, `imgdata` FROM `posts` LIMIT ? OFFSET ?", limit, offset)
		if err != nil {
			log.Print("DB Error: ", err)
			return
		}

		if len(posts) == 0 {
			break
		}

		for _, post := range posts {
			if isValidMime(post.Mime) {
				filename := fmt.Sprintf("../image/%d.%s", post.ID, getExtension(post.Mime))
				err := os.WriteFile(filename, post.Imgdata, 0644)
				if err != nil {
					log.Print("Could not writefile: ", err)
					continue
				}
				log.Printf("Image saved as file: %s\n", filename)
			}
		}
		offset += limit
	}
}

func isValidMime(mime string) bool {
	return mime == "image/jpeg" || mime == "image/png" || mime == "image/gif"
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

func main() {
	saveAllImages()
}
