package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
)

// ==========================================
// Constants & Paths
// ==========================================

const (
	UsersFile     = "/etc/zivpn/users.json"
	ApiKeyFile    = "/etc/zivpn/apikey"
	ApiPortFile   = "/etc/zivpn/api_port"
	DomainFile    = "/etc/zivpn/domain"
	PortFile      = "/etc/zivpn/port"
	DateLayout    = "2006-01-02"
)

// ==========================================
// Global
// ==========================================

var (
	apiKey string
	mutex  = &sync.Mutex{}
)

// User struct
type User struct {
	Password string `json:"password"`
	Expired  string `json:"expired"`
}

// ==========================================
// Helper: Load/Save Users
// ==========================================

func loadUsers() ([]User, error) {
	mutex.Lock()
	defer mutex.Unlock()

	if _, err := os.Stat(UsersFile); os.IsNotExist(err) {
		return []User{}, nil
	}

	data, err := ioutil.ReadFile(UsersFile)
	if err != nil {
		return nil, err
	}

	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func saveUsers(users []User) error {
	mutex.Lock()
	defer mutex.Unlock()

	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return err
	}
	return ioutil.WriteFile(UsersFile, data, 0644)
}

// ==========================================
// Helper: Auth Middleware
// ==========================================

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		if key != apiKey {
		 respondJSON(w, http.StatusUnauthorized, map[string]interface{}{
				"success": false,
				"message": "Invalid or missing API key",
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ==========================================
// Response Helper
// ==========================================

func respondJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(payload)
}

// ==========================================
// Handlers
// ==========================================

// POST /user/create
func createUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
		Days     int    `json:"days"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Invalid JSON",
		})
		return
	}

	if input.Password == "" || input.Days <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Password and days required",
		})
		return
	}

	users, err := loadUsers()
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to load users",
		})
		return
	}

	// Check duplicate
	for _, u := range users {
		if u.Password == input.Password {
			respondJSON(w, http.StatusConflict, map[string]interface{}{
				"success": false,
				"message": "Password already exists",
			})
			return
		}
	}

	expired := time.Now().AddDate(0, 0, input.Days).Format(DateLayout)

	newUser := User{
		Password: input.Password,
		Expired:  expired,
	}
	users = append(users, newUser)

	if err := saveUsers(users); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to save users",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    newUser,
	})
}

// GET /users (BARU)
func listUsersHandler(w http.ResponseWriter, r *http.Request) {
	users, err := loadUsers()
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to load users",
		})
		return
	}

	// Tambahkan status dinamis
	type UserWithStatus struct {
		Password string `json:"password"`
		Expired  string `json:"expired"`
		Status   string `json:"status"`
	}

	var result []UserWithStatus
	now := time.Now().Format(DateLayout)

	for _, u := range users {
		status := "active"
		if u.Expired < now {
			status = "expired"
		}
		result = append(result, UserWithStatus{
			Password: u.Password,
			Expired:  u.Expired,
			Status:   status,
		})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    result,
	})
}

// POST /user/delete (BARU)
func deleteUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Password == "" {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Password required",
		})
		return
	}

	users, err := loadUsers()
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to load users",
		})
		return
	}

	found := false
	newUsers := []User{}
	for _, u := range users {
		if u.Password == input.Password {
			found = true
			continue
		}
		newUsers = append(newUsers, u)
	}

	if !found {
		respondJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"message": "User not found",
		})
		return
	}

	if err := saveUsers(newUsers); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to save users",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "User deleted successfully",
	})
}

// POST /user/renew (BARU)
func renewUserHandler(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password string `json:"password"`
		Days     int    `json:"days"`
	}

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Password == "" || input.Days <= 0 {
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"success": false,
			"message": "Password and days required",
		})
		return
	}

	users, err := loadUsers()
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to load users",
		})
		return
	}

	found := false
	for i, u := range users {
		if u.Password == input.Password {
			found = true
			expiredTime, _ := time.Parse(DateLayout, u.Expired)
			// Jika sudah expired, mulai dari sekarang
			if expiredTime.Before(time.Now()) {
				expiredTime = time.Now()
			}
			newExpired := expiredTime.AddDate(0, 0, input.Days).Format(DateLayout)
			users[i].Expired = newExpired
			break
		}
	}

	if !found {
		respondJSON(w, http.StatusNotFound, map[string]interface{}{
			"success": false,
			"message": "User not found",
		})
		return
	}

	if err := saveUsers(users); err != nil {
}%
		respondJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"success": false,
			"message": "Failed to save users",
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": "User renewed successfully",
	})
}

// GET /info
func infoHandler(w http.ResponseWriter, r *http.Request) {
	// Public IP
	publicIP := "Unknown"
	if resp, err := http.Get("https://api.ipify.org"); err == nil {
		defer resp.Body.Close()
		if body, err := ioutil.ReadAll(resp.Body); err == nil {
			publicIP = string(body)
		}
	}

	// Read files
	domain := readFile(DomainFile)
	port := readFile(PortFile)
	service := "zivpn-udp" // bisa diganti sesuai service Anda

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data": map[string]string{
			"public_ip": publicIP,
			"port":      port,
			"service":   service,
			"domain":    domain,
		},
	})
}

func readFile(path string) string {
	if data, err := ioutil.ReadFile(path); err == nil {
		return strings.TrimSpace(string(data))
	}
	return ""
}

// ==========================================
// Main
// ==========================================

func main() {
	// Load API key
	if keyBytes, err := ioutil.ReadFile(ApiKeyFile); err == nil {
		apiKey = strings.TrimSpace(string(keyBytes))
	} else {
		log.Fatal("Cannot read API key")
	}

	// Load port
	port := "6969" // default
	if portBytes, err := ioutil.ReadFile(ApiPortFile); err == nil {
		port = strings.TrimSpace(string(portBytes))
	}

	r := mux.NewRouter()
	api := r.PathPrefix("/api").Subrouter()
	api.Use(authMiddleware)

	api.HandleFunc("/user/create", createUserHandler).Methods("POST")
	api.HandleFunc("/users", listUsersHandler).Methods("GET")
	api.HandleFunc("/user/delete", deleteUserHandler).Methods("POST")
	api.HandleFunc("/user/renew", renewUserHandler).Methods("POST")
	api.HandleFunc("/info", infoHandler).Methods("GET")

	log.Printf("API server running on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}