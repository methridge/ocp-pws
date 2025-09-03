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
	dataAge     time.Time // Track the actual observation time
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

// parseObsTimeLocal parses the obsTimeLocal string and returns a time.Time
func parseObsTimeLocal(obsTimeLocal string) (time.Time, error) {
	// Try common formats that Weather Underground might use
	formats := []string{
		"2006-01-02 3:04 PM MST",
		"2006-01-02 15:04 MST",
		"1/2/2006 3:04:05 PM",
		"1/2/2006 15:04:05",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02 15:04:05",
	}

	for _, format := range formats {
		if t, err := time.Parse(format, obsTimeLocal); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("unable to parse obsTimeLocal: %s", obsTimeLocal)
}

// isDataFresh checks if the observation data is less than 5 minutes old
func isDataFresh(obsTimeLocal string) (bool, time.Time, error) {
	obsTime, err := parseObsTimeLocal(obsTimeLocal)
	if err != nil {
		log.Printf("Warning: Could not parse obsTimeLocal '%s': %v", obsTimeLocal, err)
		// Fallback to current time if parsing fails
		return true, time.Now(), nil
	}

	now := time.Now()
	age := now.Sub(obsTime)
	isFresh := age <= 5*time.Minute

	log.Printf("Data observation time: %s, current time: %s, age: %v, fresh: %t",
		obsTime.Format("15:04:05"), now.Format("15:04:05"), age, isFresh)

	return isFresh, obsTime, nil
}

// shouldFetchNewData determines if we should fetch new weather data
func shouldFetchNewData(t time.Time) bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	// If we have no data, fetch it
	if cache.lastFetched.IsZero() {
		return true
	}

	// Check if cached data is stale (observation time > 5 minutes ago)
	if !cache.dataAge.IsZero() {
		age := t.Sub(cache.dataAge)
		if age > 5*time.Minute {
			log.Printf("Cached data is stale (age: %v), will fetch new data", age)
			return true
		}
	}

	// Don't fetch too frequently (respect a minimum interval of 30 seconds between API calls)
	timeSinceLastFetch := t.Sub(cache.lastFetched)
	if timeSinceLastFetch < 30*time.Second {
		log.Printf("Rate limiting: last fetch was %v ago, waiting...", timeSinceLastFetch)
		return false
	}

	return false
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

	// Check if the returned data is fresh
	obs := responseObject.Observations[0]
	isFresh, obsTime, err := isDataFresh(obs.ObsTimeLocal)
	if err != nil {
		log.Printf("Warning: Could not determine data freshness: %v", err)
	}

	// Cache the data with observation time
	cache.mu.Lock()
	cache.data = responseObject
	cache.lastFetched = time.Now()
	if err == nil {
		cache.dataAge = obsTime
	}
	cache.mu.Unlock()

	if !isFresh {
		return weatherCurrent{}, fmt.Errorf("API returned stale data (observation time: %s)", obs.ObsTimeLocal)
	}

	return responseObject, nil
}

// getCachedWeatherData returns cached weather data or fetches new data if needed
func getCachedWeatherData() (weatherCurrent, error) {
	now := time.Now()
	maxRetries := 3
	retryDelay := 5 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if shouldFetchNewData(now) {
			log.Printf("Attempt %d: Fetching new weather data at %s", attempt, now.Format("15:04:05"))

			data, err := fetchWeatherData()
			if err != nil {
				if strings.Contains(err.Error(), "stale data") && attempt < maxRetries {
					log.Printf("Got stale data, retrying in %v (attempt %d/%d)", retryDelay, attempt, maxRetries)
					time.Sleep(retryDelay)
					now = time.Now()
					continue
				}
				return weatherCurrent{}, err
			}
			return data, nil
		}

		// Return cached data if it's still fresh
		cache.mu.RLock()
		if !cache.lastFetched.IsZero() && !cache.dataAge.IsZero() {
			age := now.Sub(cache.dataAge)
			if age <= 5*time.Minute {
				log.Printf("Using cached weather data (observation age: %v)", age)
				data := cache.data
				cache.mu.RUnlock()
				return data, nil
			}
		}
		cache.mu.RUnlock()

		// If we reach here, cached data is stale, force a fetch
		log.Printf("Cached data is stale, forcing fetch")
		data, err := fetchWeatherData()
		if err != nil {
			if strings.Contains(err.Error(), "stale data") && attempt < maxRetries {
				log.Printf("Got stale data, retrying in %v (attempt %d/%d)", retryDelay, attempt, maxRetries)
				time.Sleep(retryDelay)
				now = time.Now()
				continue
			}
			return weatherCurrent{}, err
		}
		return data, nil
	}

	return weatherCurrent{}, fmt.Errorf("failed to get fresh weather data after %d attempts", maxRetries)
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
