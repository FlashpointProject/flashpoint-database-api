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
	LegacyPath     string   `json:"legacyPath"`
	ImagePath      string   `json:"imagePath"`
	ErrorImageFile string   `json:"errorImageFile"`
	LogFile        string   `json:"logFile"`
	LogActivity    bool     `json:"logActivity"`
	SearchLimit    int      `json:"searchLimit"`
	Filter         []string `json:"filter"`
}

type Fields struct {
	Names      []string
	Columns    []string
	Queries    []string
	NeedsMerge []bool
	Types      []int
}

var fields = Fields{
	[]string{"id", "library", "title", "alternateTitles", "series", "developer", "publisher", "source", "tags", "platform", "playMode", "status", "version", "releaseDate", "language", "notes", "originalDescription", "applicationPath", "launchCommand", "dateAdded", "dateModified", "zipped"},
	[]string{"id", "library", "title", "alternateTitles", "series", "developer", "publisher", "source", "tagsStr", "platformsStr", "playMode", "status", "version", "releaseDate", "language", "notes", "originalDescription", "applicationPath", "launchCommand", "dateAdded", "dateModified", "activeDataOnDisk"},
	[]string{"game.id", "game.library", "game.title", "game.alternateTitles", "game.series", "game.developer", "game.publisher", "game.source", "game.tagsStr", "game.platformsStr", "game.playMode", "game.status", "game.version", "game.releaseDate", "game.language", "game.notes", "game.originalDescription", "coalesce(game_data.applicationPath, game.applicationPath) AS applicationPath", "coalesce(game_data.launchCommand, game.launchCommand) AS launchCommand", "game.dateAdded", "game.dateModified", `CASE WHEN activeDataId ISNULL THEN "false" ELSE "true" END AS activeDataOnDisk`},
	[]bool{false, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false, false, true, true, false, false, true},
	[]int{0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2},
}
var queryReplacer = strings.NewReplacer("^", "^^", "%", "^%", "_", "^_")
var tagsIndex = slices.Index(fields.Names, "tags")

type AddApp struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ApplicationPath string `json:"applicationPath"`
	LaunchCommand   string `json:"launchCommand"`
	RunBefore       bool   `json:"runBefore"`
}

type Tag struct {
	Aliases  []string `json:"aliases"`
	Category string   `json:"category"`
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
	http.HandleFunc("/tags", tagsHandler)
	http.HandleFunc("/platforms", platformsHandler)
	http.HandleFunc("/files", filesHandler)
	http.HandleFunc("/stats", statsHandler)
	http.HandleFunc("/get", getHandler)
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
	setSharedHeadersAndLog(w, r, true)

	jsonObjects := make([]string, 0)

	urlQuery := r.URL.Query()
	operator := " AND "
	if strings.ToLower(urlQuery.Get("any")) == "true" {
		operator = " OR "
	}

	whereLike := make([]string, 0)
	whereVal := make([]string, 0)
	param := 1

	if len(urlQuery.Get("smartSearch")) > 0 {
		for _, v := range strings.Split(urlQuery.Get("smartSearch"), ",") {
			smartLike := make([]string, 0)
			for _, s := range []int{2, 3, 4, 5, 6} {
				smartLike = append(smartLike, fmt.Sprintf("%s LIKE $%d ESCAPE '^'", fields.Names[s], param))
			}
			whereVal = append(whereVal, "%"+queryReplacer.Replace(v)+"%")
			whereLike = append(whereLike, "("+strings.Join(smartLike, " OR ")+")")
			param++
		}
	}
	for i, c := range fields.Names {
		metaLike := make([]string, 0)
		if len(urlQuery.Get(c)) > 0 {
			for _, v := range strings.Split(urlQuery.Get(c), ",") {
				metaLike = append(metaLike, fmt.Sprintf("%s LIKE $%d ESCAPE '^'", fields.Columns[i], param))
				whereVal = append(whereVal, "%"+queryReplacer.Replace(v)+"%")
				param++
			}
		}
		if len(metaLike) > 0 {
			whereLike = append(whereLike, "("+strings.Join(metaLike, operator)+")")
		}
	}

	if len(whereVal) > 0 {
		outputIndices := make([]int, 0)
		if urlQuery.Has("fields") {
			for _, c := range strings.Split(urlQuery.Get("fields"), ",") {
				if i := slices.Index(fields.Names, strings.ToLower(c)); i != -1 {
					outputIndices = append(outputIndices, i)
				}
			}
		}
		if len(outputIndices) == 0 {
			for i := range fields.Names {
				outputIndices = append(outputIndices, i)
			}
		}

		outputTagsIndex := -1
		outputTagsAppend := false
		if outputTagsIndex = slices.Index(outputIndices, tagsIndex); outputTagsIndex == -1 {
			outputIndices = append(outputIndices, tagsIndex)
			outputTagsIndex = len(outputIndices) - 1
			outputTagsAppend = true
		}

		outputFields := make([]string, 0)
		for _, i := range outputIndices {
			outputFields = append(outputFields, fields.Columns[i])
		}

		var dbQuery string
		for _, i := range outputIndices {
			if fields.NeedsMerge[i] {
				dbQuery = fmt.Sprintf(`SELECT %s FROM (SELECT %s FROM game LEFT JOIN game_data ON game.id=game_data.gameId) WHERE %s`, strings.Join(outputFields, ","), strings.Join(fields.Queries, ","), strings.Join(whereLike, operator))
				break
			}
		}
		if dbQuery == "" {
			dbQuery = fmt.Sprintf(`SELECT %s FROM game WHERE %s`, strings.Join(outputFields, ","), strings.Join(whereLike, operator))
		}

		limit := config.SearchLimit
		if urlQuery.Has("limit") {
			i, err := strconv.Atoi(urlQuery.Get("limit"))
			if err == nil && i > 0 && (config.SearchLimit == -1 || i < limit) {
				limit = i
			}
		}
		if limit > 0 {
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
			entry := make([]string, len(outputIndices))
			pipe := make([]interface{}, len(outputIndices))
			for i := range pipe {
				pipe[i] = &entry[i]
			}

			err := rows.Scan(pipe...)
			if err != sql.ErrNoRows && err != nil {
				errorLog.Println(err)
				break
			}

			tags := strings.Split(entry[outputTagsIndex], "; ")
			filtered := false
			if strings.ToLower(urlQuery.Get("filter")) == "true" {
				for _, v := range tags {
					if slices.Contains(config.Filter, v) {
						filtered = true
						break
					}
				}
			}

			if !filtered && urlQuery.Has("tagsStr") {
				if queryTags := strings.Split(urlQuery.Get("tagsStr"), ","); len(queryTags) > 0 {
					tagsLower := make([]string, len(tags))
					for _, t := range tags {
						tagsLower = append(tagsLower, strings.ToLower(t))
					}
					for _, t := range queryTags {
						containsTag := slices.Contains(tagsLower, strings.ToLower(t))
						if operator == " AND " && !containsTag {
							filtered = true
							break
						} else if operator == " OR " && containsTag {
							break
						}
					}
				}
			}

			if filtered {
				continue
			} else if outputTagsAppend {
				entry = entry[:len(entry)-1]
			}

			jsonObject := "{"
			for i, v := range entry {
				fieldIndex := outputIndices[i]
				if jsonValue, err := json.Marshal(v); err == nil {
					jsonObject += `"` + fields.Names[fieldIndex] + `":`
					switch fields.Types[fieldIndex] {
					case 1:
						jsonObject += "[" + strings.ReplaceAll(string(jsonValue), "; ", `","`) + "]"
					case 2:
						jsonObject += strings.Trim(string(jsonValue), `"`)
					default:
						jsonObject += string(jsonValue)
					}
				}
				if i != len(entry)-1 {
					jsonObject += ","
				}
			}
			jsonObject += "}"

			jsonObjects = append(jsonObjects, jsonObject)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte("[" + strings.Join(jsonObjects, ",") + "]"))
}

func addAppsHandler(w http.ResponseWriter, r *http.Request) {
	setSharedHeadersAndLog(w, r, true)

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

func tagsHandler(w http.ResponseWriter, r *http.Request) {
	setSharedHeadersAndLog(w, r, true)

	tags := make([]Tag, 0)

	rows, err := db.Query("SELECT tag_alias_concat.aliases, tag_category.name FROM (SELECT id, group_concat(name, '; ') AS aliases FROM tag_alias GROUP BY tagId) tag_alias_concat JOIN tag, tag_category ON tag_alias_concat.id = tag.primaryAliasId AND tag.categoryId = tag_category.id")
	if err != nil {
		errorLog.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	for rows.Next() {
		var tag Tag
		var aliases string

		err := rows.Scan(&aliases, &tag.Category)
		if err != sql.ErrNoRows && err != nil {
			errorLog.Println(err)
			break
		}

		tag.Aliases = strings.Split(aliases, "; ")

		tags = append(tags, tag)
	}

	marshalAndWrite(tags, w)
}

func platformsHandler(w http.ResponseWriter, r *http.Request) {
	setSharedHeadersAndLog(w, r, true)

	platforms := make([]string, 0)

	rows, err := db.Query("SELECT name FROM platform_alias")
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
	setSharedHeadersAndLog(w, r, true)

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
	setSharedHeadersAndLog(w, r, true)

	var stats Stats

	queries := [3]string{
		"SELECT library, COUNT(*) FROM game GROUP BY library",
		"SELECT CASE WHEN activeDataId ISNULL THEN 'legacy' ELSE 'gameZip' END AS format, COUNT(*) FROM game GROUP BY format",
		"SELECT platform_alias.name, COUNT(*) FROM game_platforms_platform JOIN platform_alias ON game_platforms_platform.platformId = platform_alias.platformId GROUP BY game_platforms_platform.platformId",
	}
	totals := [3]*[]ColumnStats{
		&stats.LibraryTotals,
		&stats.FormatTotals,
		&stats.PlatformTotals,
	}

	for i := 0; i < 3; i++ {
		*totals[i] = make([]ColumnStats, 0)

		rows, err := db.Query(queries[i])
		if err != nil {
			errorLog.Println(err)
			w.WriteHeader(http.StatusInternalServerError)
		}

		for rows.Next() {
			var columnStats ColumnStats

			err := rows.Scan(&columnStats.Name, &columnStats.Count)
			if err != sql.ErrNoRows && err != nil {
				errorLog.Println(err)
				break
			}

			*totals[i] = append(*totals[i], columnStats)
		}
	}

	marshalAndWrite(stats, w)
}

func getHandler(w http.ResponseWriter, r *http.Request) {
	setSharedHeadersAndLog(w, r, false)

	urlQuery := r.URL.Query()

	if urlQuery.Has("id") {
		var gameZip string

		row := db.QueryRow("SELECT path FROM game_data WHERE gameId = ?", urlQuery.Get("id"))
		if err := row.Scan(&gameZip); err == nil {
			if gameZipData, err := os.ReadFile(filepath.Join(config.GameZipPath, gameZip)); err == nil {
				w.Header().Set("Content-Type", "application/zip")
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", gameZip))

				w.Write(gameZipData)
				return
			} else {
				errorLog.Println(err)
			}
		} else if err != sql.ErrNoRows {
			errorLog.Println(err)
		}
	} else if urlQuery.Has("url") {
		url := strings.TrimPrefix(urlQuery.Get("url"), "http://")
		file := filepath.Join(config.LegacyPath, strings.ReplaceAll(url, "/", string(os.PathSeparator)))

		if strings.HasPrefix(file, config.LegacyPath) {
			if fileData, err := os.ReadFile(file); err == nil {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", url[strings.LastIndex(url, "/")+1:]))

				w.Write(fileData)
				return
			} else {
				errorLog.Println(err)
			}
		}
	} else {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	w.WriteHeader(http.StatusNotFound)
}

func imageHandler(w http.ResponseWriter, r *http.Request) {
	setSharedHeadersAndLog(w, r, false)

	urlQuery := r.URL.Query()

	var imageDir string
	if strings.HasPrefix(r.URL.Path, "/logo") {
		imageDir = "Logos"
	} else if strings.HasPrefix(r.URL.Path, "/screenshot") {
		imageDir = "Screenshots"
	}

	var imageFile string
	if id := urlQuery.Get("id"); len(id) == 36 && !strings.ContainsAny(id, "/\\.") {
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

func setSharedHeadersAndLog(w http.ResponseWriter, r *http.Request, isJson bool) {
	if isJson {
		w.Header().Set("Content-Type", "application/json")
	}
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
