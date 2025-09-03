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
	"strconv"
	"strings"
	"sync"
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

// Configurable buffer time (default 30 seconds)
var fetchBufferSeconds = 30

// Cache for weather data
type weatherCache struct {
	data        weatherCurrent
	lastFetched time.Time
	mu          sync.RWMutex
}

var cache = &weatherCache{}

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

	// Configure fetch buffer (priority: env var > file > default)
	if envBuffer := os.Getenv("FETCH_BUFFER_SECONDS"); envBuffer != "" {
		if buffer, err := strconv.Atoi(envBuffer); err == nil && buffer > 0 {
			fetchBufferSeconds = buffer
			log.Printf("Using fetch buffer from env var: %d seconds", fetchBufferSeconds)
		} else {
			log.Printf("Invalid FETCH_BUFFER_SECONDS env var, using default: %d seconds", fetchBufferSeconds)
		}
	} else if bufferContent, err := os.ReadFile("/mnt/secrets/fetch_buffer"); err == nil {
		if buffer, parseErr := strconv.Atoi(strings.TrimSpace(string(bufferContent))); parseErr == nil && buffer > 0 {
			fetchBufferSeconds = buffer
			log.Printf("Using fetch buffer from file: %d seconds", fetchBufferSeconds)
		} else {
			log.Printf("Invalid fetch_buffer file format, using default: %d seconds", fetchBufferSeconds)
		}
	} else {
		log.Printf("Using default fetch buffer: %d seconds", fetchBufferSeconds)
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

// isWithinFetchWindow checks if the current time is within the fetch window
// The fetch window is from the 5-minute boundary up to fetchBufferSeconds after
func isWithinFetchWindow(t time.Time) bool {
	// Get the current 5-minute window start
	windowStart := t.Truncate(5 * time.Minute)

	// Calculate seconds since the window start
	secondsSinceWindow := int(t.Sub(windowStart).Seconds())

	// Allow fetching from 0 to fetchBufferSeconds after each 5-minute boundary
	return secondsSinceWindow >= 0 && secondsSinceWindow <= fetchBufferSeconds
}

// shouldFetchNewData determines if we should fetch new weather data
func shouldFetchNewData(t time.Time) bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	// If we have no data, fetch it
	if cache.lastFetched.IsZero() {
		return true
	}

	// Only fetch within the allowed window (5-min boundary + 30 seconds)
	if !isWithinFetchWindow(t) {
		return false
	}

	// Check if we haven't fetched data for this 5-minute window yet
	lastFetchWindow := cache.lastFetched.Truncate(5 * time.Minute)
	currentWindow := t.Truncate(5 * time.Minute)

	return currentWindow.After(lastFetchWindow)
}

// fetchWeatherData fetches weather data from the API and caches it
func fetchWeatherData() (weatherCurrent, error) {
	url := fmt.Sprintf("%s?stationId=%s&format=json&units=%s&apiKey=%s",
		api,
		sid,
		units,
		key,
	)

	log.Printf("Making API request to: %s", strings.Replace(url, key, "***REDACTED***", 1))

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return weatherCurrent{}, fmt.Errorf("error creating HTTP request: %v", err)
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return weatherCurrent{}, fmt.Errorf("error making HTTP request: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("API response status: %d %s", resp.StatusCode, resp.Status)

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return weatherCurrent{}, fmt.Errorf("error reading response body: %v", err)
	}

	log.Printf("API response body length: %d bytes", len(bodyBytes))
	if len(bodyBytes) > 0 {
		log.Printf("Raw API response: %s", string(bodyBytes))
	}

	var responseObject weatherCurrent
	if err := json.Unmarshal(bodyBytes, &responseObject); err != nil {
		log.Printf("Error unmarshaling JSON: %v", err)
		log.Printf("Raw response that failed to unmarshal: %s", string(bodyBytes))
		return weatherCurrent{}, fmt.Errorf("error parsing API response: %v", err)
	}

	log.Printf("Number of observations in response: %d", len(responseObject.Observations))

	if len(responseObject.Observations) == 0 {
		return weatherCurrent{}, fmt.Errorf("no observations found in API response")
	}

	// Cache the data
	cache.mu.Lock()
	cache.data = responseObject
	cache.lastFetched = time.Now()
	cache.mu.Unlock()

	return responseObject, nil
}

// getCachedWeatherData returns cached weather data or fetches new data if needed
func getCachedWeatherData() (weatherCurrent, error) {
	now := time.Now()

	if shouldFetchNewData(now) {
		log.Printf("Fetching new weather data at %s (within %d-second buffer window)",
			now.Format("15:04:05"), fetchBufferSeconds)
		return fetchWeatherData()
	}

	// Return cached data
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	if cache.lastFetched.IsZero() {
		// This shouldn't happen due to shouldFetchNewData logic, but handle it anyway
		cache.mu.RUnlock()
		return fetchWeatherData()
	}

	windowStart := now.Truncate(5 * time.Minute)
	secondsSinceWindow := int(now.Sub(windowStart).Seconds())
	log.Printf("Using cached weather data from: %s (current time: %s, %d seconds after 5-min boundary)",
		cache.lastFetched.Format(time.RFC3339), now.Format("15:04:05"), secondsSinceWindow)
	return cache.data, nil
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
			log.Printf("Error reading random secret: %v", err)
			http.Error(w, "Configuration error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Add cache control headers to prevent caching
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		// Get weather data (cached or fresh)
		responseObject, err := getCachedWeatherData()
		if err != nil {
			log.Printf("Error getting weather data: %v", err)
			http.Error(w, "Weather data unavailable", http.StatusServiceUnavailable)
			return
		}

		if _, ok := os.LookupEnv("DEBUG"); ok {
			fmt.Fprintf(w, "API Response as struct %+v\n", responseObject)
		}

		obs := responseObject.Observations[0]
		log.Printf("Processing observation from station: %s, time: %s", obs.StationID, obs.ObsTimeLocal)
		var feelsLikeF, feelsLikeC int
		if obs.Imperial.Temp > 70 {
			feelsLikeF = obs.Imperial.HeatIndex
			feelsLikeC = (((obs.Imperial.HeatIndex - 32) * 5) / 9)
		} else {
			feelsLikeF = obs.Imperial.WindChill
			feelsLikeC = (((obs.Imperial.WindChill - 32) * 5) / 9)
		}
		compassDirs := []string{"N", "NNE", "NE", "ENE", "E", "ESE", "SE", "SSE", "S", "SSW", "SW", "WSW", "W", "WNW", "NW", "NNW", "N"}
		compassIndex := obs.Winddir / 22

		// Ensure compass index is within bounds
		if compassIndex >= len(compassDirs) {
			compassIndex = len(compassDirs) - 1
		}

		index := Index{
			obs.StationID,
			obs.ObsTimeLocal,
			obs.Imperial.Temp,
			(((obs.Imperial.Temp - 32) * 5) / 9),
			feelsLikeF,
			feelsLikeC,
			obs.Imperial.Dewpt,
			(((obs.Imperial.Dewpt - 32) * 5) / 9),
			obs.Humidity,
			obs.Imperial.WindSpeed,
			obs.Imperial.WindGust,
			compassDirs[compassIndex],
			obs.Winddir,
			rsec,
		}

		// Parse template from embedded files
		tmpl, err := template.ParseFS(templateFiles, "templates/index.html")
		if err != nil {
			log.Printf("Error parsing template: %v", err)
			http.Error(w, "Template parsing error: "+err.Error(), http.StatusInternalServerError)
			return
		}

		if err := tmpl.ExecuteTemplate(w, "index.html", index); err != nil {
			log.Printf("Error executing template: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	log.Println("Starting server on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
