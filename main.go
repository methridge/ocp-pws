package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"
)

//go:embed static/*
var staticFiles embed.FS

//go:embed templates/*
var templateFiles embed.FS

type weatherCurrent struct {
	Observations []struct {
		StationID         string      `json:"stationID"`
		ObsTimeUtc        time.Time   `json:"obsTimeUtc"`
		ObsTimeLocal      string      `json:"obsTimeLocal"`
		Neighborhood      string      `json:"neighborhood"`
		SoftwareType      string      `json:"softwareType"`
		Country           string      `json:"country"`
		SolarRadiation    float64     `json:"solarRadiation"`
		Lon               float64     `json:"lon"`
		RealtimeFrequency interface{} `json:"realtimeFrequency"`
		Epoch             int         `json:"epoch"`
		Lat               float64     `json:"lat"`
		Uv                float64     `json:"uv"`
		Winddir           int         `json:"winddir"`
		Humidity          int         `json:"humidity"`
		QcStatus          int         `json:"qcStatus"`
		Imperial          struct {
			Temp        int     `json:"temp"`
			HeatIndex   int     `json:"heatIndex"`
			Dewpt       int     `json:"dewpt"`
			WindChill   int     `json:"windChill"`
			WindSpeed   int     `json:"windSpeed"`
			WindGust    int     `json:"windGust"`
			Pressure    float64 `json:"pressure"`
			PrecipRate  float64 `json:"precipRate"`
			PrecipTotal float64 `json:"precipTotal"`
			Elev        int     `json:"elev"`
		} `json:"imperial"`
	} `json:"observations"`
}

// Index holds fields displayed on the index.html template
type Index struct {
	StationID    string
	ReportTime   string
	CurrentTempF int
	CurrentTempC int
	FeelsLikeF   int
	FeelsLikeC   int
	DewPointF    int
	DewPointC    int
	Humidity     int
	WindSpeed    int
	WindGust     int
	WindDirC     string
	WindDirD     int
	RandomSecret string
}

var httpClient = &http.Client{
	Timeout: 10 * time.Second,
}

// Global variables for API configuration read at startup
var api, sid, units, key string

func readAPIConfig() error {
	secretFiles := map[string]*string{
		"api":   &api,
		"sid":   &sid,
		"units": &units,
		"key":   &key,
	}

	for fileName, envVar := range secretFiles {
		filePath := fmt.Sprintf("/mnt/secrets/%s", fileName)
		content, err := os.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("failed to read secret file %s: %v", fileName, err)
		}
		*envVar = strings.TrimSpace(string(content))
	}
	return nil
}

func readRandomSecret() (string, error) {
	content, err := os.ReadFile("/mnt/secrets/rsec")
	if err != nil {
		return "", fmt.Errorf("failed to read random_secret file: %v", err)
	}
	return strings.TrimSpace(string(content)), nil
}

func main() {
	// Read API configuration at startup
	if err := readAPIConfig(); err != nil {
		log.Fatal("Configuration error:", err)
	}

	// Debug printing of Environment
	if _, ok := os.LookupEnv("DEBUG"); ok {
		for _, element := range os.Environ() {
			variable := strings.Split(element, "=")
			fmt.Println(variable[0], "=>", variable[1])
		}
	}

	// Serve embedded static files
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal("Failed to create static file system:", err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Read only the random secret on each request
		rsec, err := readRandomSecret()
		if err != nil {
			http.Error(w, "Configuration error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Add cache control headers to prevent caching
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		url := fmt.Sprintf("%s?stationId=%s&format=json&units=%s&apiKey=%s",
			api,
			sid,
			units,
			key,
		)

		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			fmt.Print(err.Error())
		}
		req.Header.Add("Accept", "application/json")
		req.Header.Add("Content-Type", "application/json")
		resp, err := httpClient.Do(req)
		if err != nil {
			fmt.Print(err.Error())
		}
		defer resp.Body.Close()
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			fmt.Print(err.Error())
		}
		var responseObject weatherCurrent
		json.Unmarshal(bodyBytes, &responseObject)
		if _, ok := os.LookupEnv("DEBUG"); ok {
			fmt.Fprintf(w, "API Response as struct %+v\n", responseObject)
		}
		var feelsLikeF, feelsLikeC int
		if responseObject.Observations[0].Imperial.Temp > 70 {
			feelsLikeF = responseObject.Observations[0].Imperial.HeatIndex
			feelsLikeC = (((responseObject.Observations[0].Imperial.HeatIndex - 32) * 5) / 9)
		} else {
			feelsLikeF = responseObject.Observations[0].Imperial.WindChill
			feelsLikeC = (((responseObject.Observations[0].Imperial.WindChill - 32) * 5) / 9)
		}
		compassDirs := []string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW", "N"}
		compassIndex := responseObject.Observations[0].Winddir / 22
		index := Index{
			responseObject.Observations[0].StationID,
			responseObject.Observations[0].ObsTimeLocal,
			responseObject.Observations[0].Imperial.Temp,
			(((responseObject.Observations[0].Imperial.Temp - 32) * 5) / 9),
			feelsLikeF,
			feelsLikeC,
			responseObject.Observations[0].Imperial.Dewpt,
			(((responseObject.Observations[0].Imperial.Dewpt - 32) * 5) / 9),
			responseObject.Observations[0].Humidity,
			responseObject.Observations[0].Imperial.WindSpeed,
			responseObject.Observations[0].Imperial.WindGust,
			compassDirs[compassIndex],
			responseObject.Observations[0].Winddir,
			rsec,
		}

		// Parse template from embedded files
		tmpl, err := template.ParseFS(templateFiles, "templates/index.html")
		if err != nil {
			http.Error(w, "Template parsing error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if err := tmpl.ExecuteTemplate(w, "index.html", index); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	fmt.Println(http.ListenAndServe(":8080", nil))
}
