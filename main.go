package main

import (
	"archive/zip"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"io"
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
	GameZipPath    string   `json:"gameZipPath"`
	ImagePath      string   `json:"imagePath"`
	ErrorImageFile string   `json:"errorImageFile"`
	LogFile        string   `json:"logFile"`
	LogActivity    bool     `json:"logActivity"`
	SearchLimit    int      `json:"searchLimit"`
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

type Stats struct {
	LibraryTotals  []ColumnStats `json:"libraryTotals"`
	FormatTotals   []ColumnStats `json:"formatTotals"`
	PlatformTotals []ColumnStats `json:"platformTotals"`
}
type ColumnStats struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

var (
	serverLog *log.Logger
	errorLog  *log.Logger
	config    Config
	db        *sql.DB
)

func main() {
	configRaw, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatal("cannot read config.json")
	} else if err := json.Unmarshal([]byte(configRaw), &config); err != nil {
		log.Fatal("cannot parse config.json")
	} else {
		log.Println("loaded config.json")
	}

	var dbErr error
	db, dbErr = sql.Open("sqlite3", config.DatabasePath)
	if dbErr != nil {
		log.Fatal(dbErr)
	}

	defer db.Close()
	log.Println("connected to Flashpoint database")

	var errorOutput io.Writer
	var serverOutput io.Writer
	if config.LogFile != "" {
		if tempOutput, err := os.OpenFile(config.LogFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0666); err == nil {
			errorOutput = tempOutput
			log.Println("logging to " + config.LogFile)
		} else {
			errorOutput = os.Stdout
			log.Println("cannot open " + config.LogFile + "; logging to standard output instead")
		}

		if config.LogActivity {
			serverOutput = errorOutput
		} else {
			serverOutput = io.Discard
		}
	} else {
		errorOutput = os.Stdout
		log.Println("logging to standard output")

		if config.LogActivity {
			serverOutput = os.Stdout
		} else {
			serverOutput = io.Discard
		}
	}

	errorLog = log.New(errorOutput, "error: ", log.Ldate|log.Ltime|log.Lshortfile)
	serverLog = log.New(serverOutput, "server: ", log.Ldate|log.Ltime)

	http.HandleFunc("/search", searchHandler)
	http.HandleFunc("/addapps", addAppsHandler)
	http.HandleFunc("/platforms", platformsHandler)
	http.HandleFunc("/files", filesHandler)
	http.HandleFunc("/stats", statsHandler)
	http.HandleFunc("/logo", imageHandler)
	http.HandleFunc("/screenshot", imageHandler)

	server := &http.Server{
		Addr:         "127.0.0.1:8986",
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	serverLog.Printf("server started at %v\n", server.Addr)
	errorLog.Fatal(server.ListenAndServe())
}

func searchHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	setSharedHeadersAndLog(w, r)

	entries := make([]Entry, 0)
	data := []string{"id", "title", "alternateTitles", "series", "developer", "publisher", "dateAdded", "dateModified", "platform", "playMode", "status", "notes", "source", "applicationPath", "launchCommand", "releaseDate", "version", "originalDescription", "language", "library", "activeDataOnDisk", "tagsStr"}
	urlQuery := r.URL.Query()

	whereLike := make([]string, 0)
	whereVal := make([]string, 0)
	i := 1

	for _, c := range data {
		if len(urlQuery.Get(c)) > 0 {
			for _, v := range strings.Split(urlQuery.Get(c), ",") {
				whereLike = append(whereLike, fmt.Sprintf("%s LIKE $%d", c, i))
				whereVal = append(whereVal, "%"+v+"%")
				i++
			}
		}
	}

	if len(whereVal) > 0 {
		operator := " AND "
		if strings.ToLower(urlQuery.Get("any")) == "true" {
			operator = " OR "
		}

		dbQuery := fmt.Sprintf("SELECT %s FROM game WHERE %s", strings.Join(data, ", "), strings.Join(whereLike, operator))

		limit := config.SearchLimit
		if urlQuery.Has("limit") {
			i, err := strconv.Atoi(urlQuery.Get("limit"))
			if err == nil && i > 0 && i < limit {
				limit = i
			}
		}
		if limit > 0 && config.SearchLimit > 0 {
			dbQuery += fmt.Sprintf(" LIMIT %d", limit)
		}

		args := make([]interface{}, len(whereVal))
		for i, v := range whereVal {
			args[i] = v
		}

		rows, err := db.Query(dbQuery, args...)
		if err != nil {
			errorLog.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		for rows.Next() {
			var entry Entry
			var tagsStr string

			err := rows.Scan(&entry.ID, &entry.Title, &entry.AlternateTitles, &entry.Series, &entry.Developer, &entry.Publisher, &entry.DateAdded, &entry.DateModified, &entry.Platform, &entry.PlayMode, &entry.Status, &entry.Notes, &entry.Source, &entry.ApplicationPath, &entry.LaunchCommand, &entry.ReleaseDate, &entry.Version, &entry.OriginalDescription, &entry.Language, &entry.Library, &entry.Zipped, &tagsStr)
			if err != sql.ErrNoRows && err != nil {
				errorLog.Println(err)
				break
			}

			entry.Tags = strings.Split(tagsStr, "; ")

			filtered := false
			if strings.ToLower(urlQuery.Get("filter")) == "true" {
				for _, v := range entry.Tags {
					if slices.Contains(config.Filter, v) {
						filtered = true
						break
					}
				}
			}

			tags := strings.Split(urlQuery.Get("tagsStr"), ",")
			if len(tags) > 0 {
				entryTagsLower := make([]string, len(entry.Tags))
				for i := range entry.Tags {
					entryTagsLower = append(entryTagsLower, strings.ToLower(entry.Tags[i]))
				}

				for _, v := range tags {
					containsTag := slices.Contains(entryTagsLower, strings.ToLower(v))
					if operator == " AND " && !containsTag {
						filtered = true
						break
					} else if operator == " OR " && containsTag {
						break
					}
				}
			}

			if !filtered {
				entries = append(entries, entry)
			}
		}
	}

	marshalAndWrite(entries, w)
}

func addAppsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	setSharedHeadersAndLog(w, r)

	addApps := make([]AddApp, 0)
	urlQuery := r.URL.Query()

	if urlQuery.Has("id") {
		rows, err := db.Query("SELECT id, applicationPath, autoRunBefore, launchCommand, name FROM additional_app WHERE parentGameId = ?", urlQuery.Get("id"))
		if err != nil {
			errorLog.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		for rows.Next() {
			var addApp AddApp

			err := rows.Scan(&addApp.ID, &addApp.ApplicationPath, &addApp.RunBefore, &addApp.LaunchCommand, &addApp.Name)
			if err != sql.ErrNoRows && err != nil {
				errorLog.Println(err)
				break
			}

			addApps = append(addApps, addApp)
		}
	}

	marshalAndWrite(addApps, w)
}

func platformsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	setSharedHeadersAndLog(w, r)

	platforms := make([]string, 0)

	rows, err := db.Query("SELECT platform FROM game GROUP BY platform")
	if err != nil {
		errorLog.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for rows.Next() {
		var platform string

		err := rows.Scan(&platform)
		if err != sql.ErrNoRows && err != nil {
			errorLog.Println(err)
			break
		}

		platforms = append(platforms, platform)
	}

	marshalAndWrite(platforms, w)
}

func filesHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	setSharedHeadersAndLog(w, r)

	files := make([]string, 0)
	urlQuery := r.URL.Query()

	if urlQuery.Has("id") {
		var gameZip string

		row := db.QueryRow("SELECT path FROM game_data WHERE gameId = ?", urlQuery.Get("id"))
		if err := row.Scan(&gameZip); err == nil {
			zip, err := zip.OpenReader(filepath.Join(config.GameZipPath, gameZip))
			if err == nil {
				defer zip.Close()

				for _, file := range zip.File {
					if strings.HasPrefix(file.Name, "content/") {
						files = append(files, strings.TrimPrefix(file.Name, "content/"))
					}
				}
			} else {
				errorLog.Println(err)
			}
		} else if err != sql.ErrNoRows {
			errorLog.Println(err)
		}
	}

	marshalAndWrite(files, w)
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	setSharedHeadersAndLog(w, r)

	var stats Stats

	stats.LibraryTotals = make([]ColumnStats, 0)
	stats.FormatTotals = make([]ColumnStats, 0)
	stats.PlatformTotals = make([]ColumnStats, 0)

	if err := addStat("library", &stats.LibraryTotals); err != http.StatusFound {
		w.WriteHeader(err)
		return
	}
	if err := addStat("platform", &stats.PlatformTotals); err != http.StatusFound {
		w.WriteHeader(err)
		return
	}

	formatRows, err := db.Query("SELECT activeDataOnDisk, COUNT(*) FROM game GROUP BY activeDataOnDisk")
	if err != nil {
		errorLog.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for formatRows.Next() {
		var formatStats ColumnStats
		var activeDataOnDisk bool

		err := formatRows.Scan(&activeDataOnDisk, &formatStats.Count)
		if err != sql.ErrNoRows && err != nil {
			errorLog.Println(err)
			break
		}

		if activeDataOnDisk {
			formatStats.Name = "gameZip"
		} else {
			formatStats.Name = "legacy"
		}

		stats.FormatTotals = append(stats.FormatTotals, formatStats)
	}

	marshalAndWrite(stats, w)
}

func imageHandler(w http.ResponseWriter, r *http.Request) {
	setSharedHeadersAndLog(w, r)

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

	imageData, _, err := image.Decode(imageRaw)
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

func setSharedHeadersAndLog(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	serverLog.Printf("serving %s to %s\n", r.URL.RequestURI(), r.Header.Get("X-Forwarded-For"))
}

func marshalAndWrite(object any, w http.ResponseWriter) {
	data, err := json.Marshal(object)
	if err != nil {
		errorLog.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write(data)
}

func addStat(column string, destination *[]ColumnStats) int {
	rows, err := db.Query(fmt.Sprintf("SELECT %[1]s, COUNT(*) FROM game GROUP BY %[1]s", column))
	if err != nil {
		errorLog.Println(err)
		return http.StatusInternalServerError
	}

	for rows.Next() {
		var stats ColumnStats

		err := rows.Scan(&stats.Name, &stats.Count)
		if err != sql.ErrNoRows && err != nil {
			errorLog.Println(err)
			break
		}

		*destination = append(*destination, stats)
	}

	return http.StatusFound
}
