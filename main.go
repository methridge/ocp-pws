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

type weatherObservation struct {
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
}

type weatherCurrent struct {
	Observations []weatherObservation `json:"observations"`
}

type wlStationsResponse struct {
	Stations []struct {
		StationID     int64  `json:"station_id"`
		StationIDUUID string `json:"station_id_uuid"`
		StationName   string `json:"station_name"`
	} `json:"stations"`
}

type wlCurrentResponse struct {
	StationID   int64      `json:"station_id"`
	Sensors     []wlSensor `json:"sensors"`
	GeneratedAt int64      `json:"generated_at"`
}

type wlSensor struct {
	LSID              int64                    `json:"lsid"`
	SensorType        int                      `json:"sensor_type"`
	DataStructureType int                      `json:"data_structure_type"`
	Data              []map[string]interface{} `json:"data"`
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
var api, key, apiSecret string

// Cached station ID
var cachedStationID int64
var stationIDMutex sync.RWMutex

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
		"api":        &api,
		"key":        &key,
		"api_secret": &apiSecret,
	}

	for fileName, envVar := range secretFiles {
		filePath := fmt.Sprintf("/mnt/secrets/%s", fileName)
		content, err := os.ReadFile(filePath)
		if err != nil {
			envKey := strings.ToUpper(fileName)
			if val := os.Getenv(envKey); val != "" {
				*envVar = val
				log.Printf("Using %s from environment variable", envKey)
				continue
			}
			return fmt.Errorf("failed to read secret file %s and env var %s is not set", fileName, envKey)
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
		if val := os.Getenv("RANDOM_SECRET"); val != "" {
			return val, nil
		}
		return "", fmt.Errorf("failed to read random_secret file: %v", err)
	}
	return strings.TrimSpace(string(content)), nil
}

// isDataFresh checks if the observation data is reasonably current
// With 30-minute fetch intervals, we accept data up to 35 minutes old
func isDataFresh(obsTimeUtc time.Time) (bool, time.Time, error) {
	now := time.Now().UTC()
	age := now.Sub(obsTimeUtc)
	isFresh := age <= 35*time.Minute

	log.Printf("Data observation time (UTC): %s, current time (UTC): %s, age: %v, fresh: %t",
		obsTimeUtc.Format("15:04:05"), now.Format("15:04:05"), age, isFresh)

	return isFresh, obsTimeUtc, nil
}

// shouldFetchNewData determines if we should fetch new weather data
func shouldFetchNewData(t time.Time) bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()

	// If we have no data, fetch it
	if cache.lastFetched.IsZero() {
		return true
	}

	// Enforce minimum 30-minute interval between API calls to avoid overloading the API
	timeSinceLastFetch := t.Sub(cache.lastFetched)
	if timeSinceLastFetch < 30*time.Minute {
		log.Printf("Rate limiting: last fetch was %v ago, must wait 30 minutes between fetches", timeSinceLastFetch)
		return false
	}

	// If we've waited 30+ minutes, fetch new data
	log.Printf("30 minutes elapsed since last fetch (%v ago), will fetch new data", timeSinceLastFetch)
	return true
}

// discoverStationID fetches and caches the station ID from the WeatherLink API
func discoverStationID() (int64, error) {
	stationIDMutex.RLock()
	if cachedStationID != 0 {
		defer stationIDMutex.RUnlock()
		return cachedStationID, nil
	}
	stationIDMutex.RUnlock()

	url := fmt.Sprintf("%s/stations?api-key=%s", api, key)

	log.Printf("Discovering station ID from: %s", strings.Replace(url, key, "***REDACTED***", 1))

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, fmt.Errorf("error creating stations request: %v", err)
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("X-Api-Secret", apiSecret)

	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("error fetching stations: %v", err)
	}
	defer resp.Body.Close()

	log.Printf("Stations API response status: %d %s", resp.StatusCode, resp.Status)

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("stations API returned status %d: %s", resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("error reading stations response: %v", err)
	}

	var stationsResp wlStationsResponse
	if err := json.Unmarshal(body, &stationsResp); err != nil {
		return 0, fmt.Errorf("error parsing stations response: %v", err)
	}

	if len(stationsResp.Stations) == 0 {
		return 0, fmt.Errorf("no stations found for this API key")
	}

	stationIDMutex.Lock()
	cachedStationID = stationsResp.Stations[0].StationID
	stationIDMutex.Unlock()

	log.Printf("Discovered station ID: %d (%s)", stationsResp.Stations[0].StationID, stationsResp.Stations[0].StationName)

	return cachedStationID, nil
}

// isISSCurrentConditions returns true for sensors that carry ISS current conditions.
// Supported combinations:
//   - type 45 / struct 10: WeatherLink Live ISS
//   - type 43 / struct 23: WeatherLink Console ISS
func isISSCurrentConditions(s wlSensor) bool {
	return (s.SensorType == 45 && s.DataStructureType == 10) ||
		(s.SensorType == 43 && s.DataStructureType == 23)
}

// convertWLToLegacy converts WeatherLink v2 response to legacy weatherCurrent format
func convertWLToLegacy(wl wlCurrentResponse) (weatherCurrent, error) {
	var issData map[string]interface{}
	var baroData map[string]interface{}
	for _, sensor := range wl.Sensors {
		if isISSCurrentConditions(sensor) && len(sensor.Data) > 0 {
			issData = sensor.Data[0]
		}
		// sensor_type 242 / data_structure_type 19 = barometric pressure sensor
		if sensor.SensorType == 242 && sensor.DataStructureType == 19 && len(sensor.Data) > 0 {
			baroData = sensor.Data[0]
		}
	}

	if issData == nil {
		return weatherCurrent{}, fmt.Errorf("no ISS current conditions data found (supported: sensor_type 45/struct 10 or 43/struct 23)")
	}

	ts, ok := issData["ts"].(float64)
	if !ok {
		return weatherCurrent{}, fmt.Errorf("missing or invalid timestamp in sensor data")
	}
	obsTime := time.Unix(int64(ts), 0).UTC()
	obsTimeLocal := obsTime.Format("2006-01-02 15:04:05 MST")

	extractFloat := func(key string) float64 {
		if v, ok := issData[key]; ok && v != nil {
			if f, ok := v.(float64); ok {
				return f
			}
		}
		return 0
	}

	extractInt := func(key string) int {
		return int(extractFloat(key))
	}

	observation := weatherObservation{
		StationID:      fmt.Sprintf("%d", wl.StationID),
		ObsTimeUtc:     obsTime,
		ObsTimeLocal:   obsTimeLocal,
		Winddir:        extractInt("wind_dir_last"),
		Humidity:       extractInt("hum"),
		Uv:             extractFloat("uv_index"),
		SolarRadiation: extractFloat("solar_rad"),
		Epoch:          int(ts),
	}

	observation.Imperial.Temp = extractInt("temp")
	observation.Imperial.HeatIndex = extractInt("heat_index")
	observation.Imperial.Dewpt = extractInt("dew_point")
	observation.Imperial.WindChill = extractInt("wind_chill")
	observation.Imperial.WindSpeed = extractInt("wind_speed_last")
	observation.Imperial.WindGust = extractInt("wind_speed_hi_last_2_min")

	if baroData != nil {
		if v, ok := baroData["bar_sea_level"]; ok && v != nil {
			if f, ok := v.(float64); ok {
				observation.Imperial.Pressure = f
			}
		}
	}

	return weatherCurrent{Observations: []weatherObservation{observation}}, nil
}

// fetchWeatherData fetches weather data from the WeatherLink v2 API and caches it
func fetchWeatherData() (weatherCurrent, error) {
	stationID, err := discoverStationID()
	if err != nil {
		return weatherCurrent{}, fmt.Errorf("failed to discover station ID: %v", err)
	}

	url := fmt.Sprintf("%s/current/%d?api-key=%s", api, stationID, key)

	log.Printf("Making WeatherLink v2 API request to: %s", strings.Replace(url, key, "***REDACTED***", 1))

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return weatherCurrent{}, fmt.Errorf("error creating HTTP request: %v", err)
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Add("X-Api-Secret", apiSecret)

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

	var wlResponse wlCurrentResponse
	if err := json.Unmarshal(bodyBytes, &wlResponse); err != nil {
		log.Printf("Error unmarshaling JSON: %v", err)
		log.Printf("Raw response that failed to unmarshal: %s", string(bodyBytes))
		return weatherCurrent{}, fmt.Errorf("error parsing API response: %v", err)
	}

	log.Printf("Response for station ID %d contains %d sensors", wlResponse.StationID, len(wlResponse.Sensors))

	responseObject, err := convertWLToLegacy(wlResponse)
	if err != nil {
		return weatherCurrent{}, err
	}

	log.Printf("Number of observations in response: %d", len(responseObject.Observations))

	if len(responseObject.Observations) == 0 {
		return weatherCurrent{}, fmt.Errorf("no observations found in API response")
	}

	obs := responseObject.Observations[0]
	isFresh, obsTime, err := isDataFresh(obs.ObsTimeUtc)
	if err != nil {
		log.Printf("Warning: Could not determine data freshness: %v", err)
	}

	cache.mu.Lock()
	cache.data = responseObject
	cache.lastFetched = time.Now()
	if err == nil {
		cache.dataAge = obsTime
	}
	cache.mu.Unlock()

	if !isFresh {
		return weatherCurrent{}, fmt.Errorf("API returned stale data (observation time UTC: %s)", obs.ObsTimeUtc.Format(time.RFC3339))
	}

	return responseObject, nil
}

// getCachedWeatherData returns cached weather data or fetches new data if needed
func getCachedWeatherData() (weatherCurrent, error) {
	now := time.Now()

	// Check if we should fetch new data (respects 30-minute minimum interval)
	if shouldFetchNewData(now) {
		log.Printf("Fetching new weather data at %s", now.Format("15:04:05"))

		data, err := fetchWeatherData()
		if err != nil {
			// If we have cached data, return it even if fetch failed
			cache.mu.RLock()
			if !cache.lastFetched.IsZero() {
				log.Printf("Fetch failed, but returning cached data from %s: %v", cache.lastFetched.Format("15:04:05"), err)
				cachedData := cache.data
				cache.mu.RUnlock()
				return cachedData, nil
			}
			cache.mu.RUnlock()
			return weatherCurrent{}, err
		}
		return data, nil
	}

	// Return cached data
	cache.mu.RLock()
	if !cache.lastFetched.IsZero() {
		timeSinceLastFetch := now.Sub(cache.lastFetched)
		log.Printf("Using cached weather data (fetched %v ago)", timeSinceLastFetch)
		data := cache.data
		cache.mu.RUnlock()
		return data, nil
	}
	cache.mu.RUnlock()

	return weatherCurrent{}, fmt.Errorf("no cached weather data available")
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
