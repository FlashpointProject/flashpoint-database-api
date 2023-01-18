package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/exp/slices"
	"golang.org/x/image/draw"
)

type Config struct {
	DatabasePath   string   `json:"databasePath"`
	ImagePath      string   `json:"imagePath"`
	ErrorImageFile string   `json:"errorImageFile"`
	Filter         []string `json:"filter"`
}

type Entry struct {
	ID                  string   `json:"id"`
	Library             string   `json:"library"`
	Title               string   `json:"title"`
	AlternateTitles     string   `json:"alternateTitles"`
	Series              string   `json:"series"`
	Developer           string   `json:"developer"`
	Publisher           string   `json:"publisher"`
	Source              string   `json:"source"`
	Tags                []string `json:"tags"`
	Platform            string   `json:"platform"`
	PlayMode            string   `json:"playMode"`
	Status              string   `json:"status"`
	Version             string   `json:"version"`
	ReleaseDate         string   `json:"releaseDate"`
	Language            string   `json:"language"`
	Notes               string   `json:"notes"`
	OriginalDescription string   `json:"originalDescription"`
	ApplicationPath     string   `json:"applicationPath"`
	LaunchCommand       string   `json:"launchCommand"`
	DateAdded           string   `json:"dateAdded"`
	DateModified        string   `json:"dateModified"`
	Zipped              bool     `json:"zipped"`
}
type AddApp struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ApplicationPath string `json:"applicationPath"`
	LaunchCommand   string `json:"launchCommand"`
	RunBefore       bool   `json:"runBefore"`
}

var config Config

var db *sql.DB
var dbErr error

func main() {
	configRaw, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatal("cannot read config.json")
	} else if err := json.Unmarshal([]byte(configRaw), &config); err != nil {
		log.Fatal("cannot parse config.json")
	} else {
		log.Print("loaded config.json")
	}

	db, dbErr = sql.Open("sqlite3", config.DatabasePath)
	if dbErr != nil {
		log.Fatal(dbErr)
	}

	defer db.Close()
	log.Println("connected to Flashpoint database")

	http.HandleFunc("/search", searchHandler)
	http.HandleFunc("/addapp", addAppHandler)
	http.HandleFunc("/logo", imageHandler)
	http.HandleFunc("/screenshot", imageHandler)

	server := &http.Server{
		Addr:         "127.0.0.1:8986",
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	log.Printf("server started at %v\n", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")
	log.Printf("serving %s to %s\n", r.URL.RequestURI(), strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])

	entries := make([]Entry, 0)
	data := []string{"id", "title", "alternateTitles", "series", "developer", "publisher", "dateAdded", "dateModified", "platform", "playMode", "status", "notes", "source", "applicationPath", "launchCommand", "releaseDate", "version", "originalDescription", "language", "library", "activeDataOnDisk", "tagsStr"}
	urlQuery := r.URL.Query()

	whereLike := make([]string, 0)
	whereVal := make([]string, 0)
	for i, v := range data {
		if urlQuery.Has(v) && len(urlQuery.Get(v)) >= 3 {
			whereLike = append(whereLike, fmt.Sprintf("%s LIKE $%d", v, i))
			whereVal = append(whereVal, "%"+urlQuery.Get(v)+"%")
		}
	}

	if len(whereVal) > 0 {
		dbQuery := fmt.Sprintf("SELECT %s FROM game WHERE %s", strings.Join(data, ", "), strings.Join(whereLike, " AND "))

		if urlQuery.Has("limit") {
			i, err := strconv.Atoi(urlQuery.Get("limit"))
			if err == nil && i > 0 {
				dbQuery += fmt.Sprintf(" LIMIT %d", i)
			}
		}

		args := make([]interface{}, len(whereVal))
		for i, v := range whereVal {
			args[i] = v
		}

		rows, err := db.Query(dbQuery, args...)
		if err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		for rows.Next() {
			var entry Entry
			var tagsStr string

			err := rows.Scan(&entry.ID, &entry.Title, &entry.AlternateTitles, &entry.Series, &entry.Developer, &entry.Publisher, &entry.DateAdded, &entry.DateModified, &entry.Platform, &entry.PlayMode, &entry.Status, &entry.Notes, &entry.Source, &entry.ApplicationPath, &entry.LaunchCommand, &entry.ReleaseDate, &entry.Version, &entry.OriginalDescription, &entry.Language, &entry.Library, &entry.Zipped, &tagsStr)
			if err != sql.ErrNoRows && err != nil {
				log.Print(err)
				break
			}

			entry.Tags = strings.Split(tagsStr, "; ")

			filtered := false
			if urlQuery.Has("filter") && strings.ToLower(urlQuery.Get("filter")) == "true" {
				for _, v := range entry.Tags {
					if slices.Contains(config.Filter, v) {
						filtered = true
						break
					}
				}
			}

			if !filtered {
				entries = append(entries, entry)
			}
		}
	}

	results, err := json.Marshal(entries)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write(results)
}

func addAppHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")
	log.Printf("serving %s to %s\n", r.URL.RequestURI(), strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])

	addApps := make([]AddApp, 0)
	urlQuery := r.URL.Query()

	if urlQuery.Has("id") {
		rows, err := db.Query("SELECT id, applicationPath, autoRunBefore, launchCommand, name FROM additional_app WHERE parentGameId = ?", urlQuery.Get("id"))
		if err != nil {
			log.Print(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		for rows.Next() {
			var addApp AddApp

			err := rows.Scan(&addApp.ID, &addApp.ApplicationPath, &addApp.RunBefore, &addApp.LaunchCommand, &addApp.Name)
			if err != sql.ErrNoRows && err != nil {
				log.Print(err)
				break
			}

			addApps = append(addApps, addApp)
		}
	}

	results, err := json.Marshal(addApps)
	if err != nil {
		log.Print(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write(results)
}

func imageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	log.Printf("serving %s to %s\n", r.URL.RequestURI(), strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])

	urlQuery := r.URL.Query()

	var imageDir string
	if strings.HasPrefix(r.URL.Path, "/logo") {
		imageDir = "Logos"
	} else if strings.HasPrefix(r.URL.Path, "/screenshot") {
		imageDir = "Screenshots"
	}

	var imageFile string
	if id := urlQuery.Get("id"); len(id) == 36 {
		imageFile = filepath.Join(config.ImagePath, imageDir, id[0:2], id[2:4], id+".png")
	} else {
		imageFile = config.ErrorImageFile
	}

	var imageRaw *os.File
	for {
		image, err := os.Open(imageFile)
		if err != nil && imageFile != config.ErrorImageFile {
			imageFile = config.ErrorImageFile
		} else if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		} else {
			imageRaw = image
			break
		}
	}
	defer imageRaw.Close()

	imageData, err := png.Decode(imageRaw)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	if urlQuery.Has("width") {
		i, err := strconv.Atoi(urlQuery.Get("width"))
		if err == nil && i > 0 && i <= imageData.Bounds().Max.X {
			width := i
			height := int(float32(imageData.Bounds().Max.Y) * (float32(width) / float32(imageData.Bounds().Max.X)))

			if height > 0 {
				imageScaled := image.NewRGBA(image.Rect(0, 0, width, height))
				draw.BiLinear.Scale(imageScaled, imageScaled.Rect, imageData, imageData.Bounds(), draw.Over, nil)
				imageData = imageScaled
			}
		}
	} else if urlQuery.Has("height") {
		i, err := strconv.Atoi(urlQuery.Get("height"))
		if err == nil && i > 0 && i <= imageData.Bounds().Max.Y {
			height := i
			width := int(float32(imageData.Bounds().Max.X) * (float32(height) / float32(imageData.Bounds().Max.Y)))

			if width > 0 {
				imageScaled := image.NewRGBA(image.Rect(0, 0, width, height))
				draw.BiLinear.Scale(imageScaled, imageScaled.Rect, imageData, imageData.Bounds(), draw.Over, nil)
				imageData = imageScaled
			}
		}
	}

	if urlQuery.Has("format") && strings.ToLower(urlQuery.Get("format")) == "jpeg" {
		w.Header().Set("Content-Type", "image/jpeg")

		quality := 80
		if urlQuery.Has("quality") {
			i, err := strconv.Atoi(urlQuery.Get("quality"))
			if err == nil && i >= 0 && i <= 100 {
				quality = i
			}
		}

		jpeg.Encode(w, imageData, &jpeg.Options{Quality: quality})
	} else {
		w.Header().Set("Content-Type", "image/png")
		png.Encode(w, imageData)
	}
}
