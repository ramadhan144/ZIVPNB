package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ==========================================
// Constants & Configuration
// ==========================================

const (
	BotConfigFile = "/etc/zivpn/bot-config.json"
	ApiPortFile   = "/etc/zivpn/api_port"
	ApiKeyFile    = "/etc/zivpn/apikey"
	DomainFile    = "/etc/zivpn/domain"
	PortFile      = "/etc/zivpn/port"
)

var ApiUrl = "http://127.0.0.1:" + PortFile + "/api"

var ApiKey = "AutoFtBot-agskjgdvsbdreiWG1234512SDKrqw"

type BotConfig struct {
	BotToken      string `json:"bot_token"`
	AdminID       int64  `json:"admin_id"`
	Mode          string `json:"mode"`
	Domain        string `json:"domain"`
	PakasirSlug   string `json:"pakasir_slug"`
	PakasirApiKey string `json:"pakasir_api_key"`
	DailyPrice    int    `json:"daily_price"`
}

type IpInfo struct {
	City string `json:"city"`
	Isp  string `json:"isp"`
}

type UserData struct {
	Password string `json:"password"`
	Expired  string `json:"expired"`
	Status   string `json:"status"`
}

type PakasirResponse struct {
	PaymentNumber string `json:"payment_number"`
	ExpiredAt     string `json:"expired_at"`
}

// ==========================================
// Global State
// ==========================================

var userStates = make(map[int64]string)
var tempUserData = make(map[int64]map[string]string)
var lastMessageIDs = make(map[int64]int)
var mutex = &sync.Mutex{}

// ==========================================
// Main Entry Point
// ==========================================

func main() {
	if keyBytes, err := ioutil.ReadFile(ApiKeyFile); err == nil {
		ApiKey = strings.TrimSpace(string(keyBytes))
	}

	if portBytes, err := ioutil.ReadFile(ApiPortFile); err == nil {
		port := strings.TrimSpace(string(portBytes))
		ApiUrl = fmt.Sprintf("http://127.0.0.1:%s/api", port)
	}

	config, err := loadConfig()
	if err != nil {
		log.Fatal("Gagal memuat konfigurasi bot:", err)
	}

	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		log.Panic(err)
	}

	bot.Debug = false
	log.Printf("Authorized on account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	go startPaymentChecker(bot, &config)

	for update := range updates {
		if update.Message != nil {
			handleMessage(bot, update.Message, &config)
		} else if update.CallbackQuery != nil {
			handleCallback(bot, update.CallbackQuery, &config)
		}
	}
}

// ==========================================
// Pakasir Payment Functions
// ==========================================

func createPakasirTransaction(config *BotConfig, orderID string, amount int) (*PakasirResponse, error) {
	apiURL := fmt.Sprintf("https://pakasir.com/api/v1/transaction/create/%s", config.PakasirSlug)

	data := url.Values{}
	data.Set("api_key", config.PakasirApiKey)
	data.Set("order_id", orderID)
	data.Set("amount", strconv.Itoa(amount))
	data.Set("payment_method", "qris")

	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	if result["success"] != true {
		return nil, fmt.Errorf("pakasir error: %v", result["message"])
	}

	dataResp := result["data"].(map[string]interface{})
	return &PakasirResponse{
		PaymentNumber: dataResp["payment_number"].(string),
		ExpiredAt:     dataResp["expired_at"].(string),
	}, nil
}

func checkPakasirStatus(config *BotConfig, orderID string) (string, error) {
	apiURL := fmt.Sprintf("https://pakasir.com/api/v1/transaction/status/%s", config.PakasirSlug)

	data := url.Values{}
	data.Set("api_key", config.PakasirApiKey)
	data.Set("order_id", orderID)

	req, _ := http.NewRequest("POST", apiURL, strings.NewReader(data.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	if result["success"] != true {
		return "", fmt.Errorf("pakasir error")
	}

	dataResp := result["data"].(map[string]interface{})
	return dataResp["status"].(string), nil
}

func startPaymentChecker(bot *tgbotapi.BotAPI, config *BotConfig) {
	ticker := time.NewTicker(60 * time.Second)
	for range ticker.C {
		mutex.Lock()
		for userID, data := range tempUserData {
			if orderID, ok := data["order_id"]; ok {
				chatID, _ := strconv.ParseInt(data["chat_id"], 10, 64)
				status, err := checkPakasirStatus(config, orderID)
				if err == nil && (status == "completed" || status == "paid") {
					password := data["password"]
					days, _ := strconv.Atoi(data["days"])
					createUser(bot, chatID, password, days, config)
					delete(tempUserData, userID)
					resetState(userID)
				}
			}
		}
		mutex.Unlock()
	}
}

func processPayment(bot *tgbotapi.BotAPI, chatID int64, userID int64, days int, config *BotConfig) {
	price := days * config.DailyPrice
	if price < 500 {
		sendMessage(bot, chatID, fmt.Sprintf("❌ Minimal transaksi Rp 500. Saat ini Rp %d.", price))
		resetState(userID)
		return
	}

	orderID := fmt.Sprintf("ZIVPN-%d-%d", userID, time.Now().Unix())

	payment, err := createPakasirTransaction(config, orderID, price)
	if err != nil {
		replyError(bot, chatID, "Gagal membuat pembayaran: "+err.Error())
		resetState(userID)
		return
	}

	mutex.Lock()
	tempUserData[userID]["order_id"] = orderID
	tempUserData[userID]["price"] = strconv.Itoa(price)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	tempUserData[userID]["days"] = strconv.Itoa(days)
	tempUserData[userID]["password"] = tempUserData[userID]["password"]
	mutex.Unlock()

	qrURL := fmt.Sprintf("https://api.qrserver.com/v1/create-qr-code/?size=300x300&data=%s", url.QueryEscape(payment.PaymentNumber))

	msgText := fmt.Sprintf("💳 **Tagihan Pembayaran**\n\nPassword: `%s`\nDurasi: %d Hari\nTotal: Rp %d\n\nScan QRIS di atas.\nPembayaran dicek otomatis.\nExpired: %s",
		tempUserData[userID]["password"], days, price, payment.ExpiredAt)

	photo := tgbotapi.NewPhoto(chatID, tgbotapi.FileURL(qrURL))
	photo.Caption = msgText
	photo.ParseMode = "Markdown"
	photo.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")),
	)

	deleteLastMessage(bot, chatID)
	bot.Send(photo)
}

// ==========================================
// Telegram Event Handlers
// ==========================================

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	if state, exists := userStates[msg.From.ID]; exists {
		handleState(bot, msg, state, config)
		return
	}

	if msg.Document != nil && msg.From.ID == config.AdminID {
		if state, exists := userStates[msg.From.ID]; exists && state == "waiting_restore_file" {
			processRestoreFile(bot, msg, config)
			return
		}
	}

	if msg.IsCommand() {
		switch msg.Command() {
		case "start":
			showMainMenu(bot, msg.Chat.ID, config)
		default:
			replyError(bot, msg.Chat.ID, "Perintah tidak dikenal.")
		}
	}
}

func handleCallback(bot *tgbotapi.BotAPI, query *tgbotapi.CallbackQuery, config *BotConfig) {
	chatID := query.Message.Chat.ID
	userID := query.From.ID

	switch {
	case query.Data == "menu_create":
		startCreateUser(bot, chatID, userID)
	case query.Data == "menu_info":
		systemInfo(bot, chatID, config)
	case query.Data == "cancel":
		cancelOperation(bot, chatID, userID, config)

	case query.Data == "menu_admin":
		if userID == config.AdminID {
			showAdminPanel(bot, chatID)
		}
	case query.Data == "admin_create":
		if userID == config.AdminID {
			startAdminCreateUser(bot, chatID, userID)
		}
	case query.Data == "admin_delete":
		if userID == config.AdminID {
			showUserSelection(bot, chatID, 1, "delete")
		}
	case query.Data == "admin_renew":
		if userID == config.AdminID {
			showUserSelection(bot, chatID, 1, "renew")
		}
	case query.Data == "admin_list":
		if userID == config.AdminID {
			listUsers(bot, chatID)
		}
	case query.Data == "menu_backup_action":
		if userID == config.AdminID {
			performBackup(bot, chatID)
		}
	case query.Data == "menu_restore_action":
		if userID == config.AdminID {
			startRestore(bot, chatID, userID)
		}

	case strings.HasPrefix(query.Data, "page_"):
		if userID == config.AdminID {
			handlePagination(bot, chatID, query.Data)
		}
	case strings.HasPrefix(query.Data, "select_renew:"):
		if userID == config.AdminID {
			startRenewUser(bot, chatID, userID, query.Data)
		}
	case strings.HasPrefix(query.Data, "select_delete:"):
		if userID == config.AdminID {
			confirmDeleteUser(bot, chatID, query.Data)
		}
	case strings.HasPrefix(query.Data, "confirm_delete:"):
		if userID == config.AdminID {
			username := strings.TrimPrefix(query.Data, "confirm_delete:")
			deleteUser(bot, chatID, username, config)
		}
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config *BotConfig) {
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	chatID := msg.Chat.ID

	switch state {
	case "create_password":
		if !validatePassword(bot, chatID, text) {
			return
		}
		mutex.Lock()
		tempUserData[userID]["password"] = text
		mutex.Unlock()
		userStates[userID] = "create_days"
		sendMessage(bot, chatID, fmt.Sprintf("⏳ Masukkan Durasi (hari)\nHarga: Rp %d / hari:", config.DailyPrice))

	case "create_days":
		days, ok := validateNumber(bot, chatID, text, 1, 365, "Durasi")
		if !ok {
			return
		}
		mutex.Lock()
		tempUserData[userID]["days"] = text
		mutex.Unlock()
		processPayment(bot, chatID, userID, days, config)

	case "admin_create_password":
		if userID != config.AdminID {
			return
		}
		if !validatePassword(bot, chatID, text) {
			return
		}
		tempUserData[userID]["password"] = text
		userStates[userID] = "admin_create_days"
		sendMessage(bot, chatID, "⏳ Masukkan Durasi (hari):")

	case "admin_create_days":
		if userID != config.AdminID {
			return
		}
		days, ok := validateNumber(bot, chatID, text, 1, 9999, "Durasi")
		if !ok {
			return
		}
		password := tempUserData[userID]["password"]
		createUser(bot, chatID, password, days, config)
		resetState(userID)

	case "admin_renew_days":
		if userID != config.AdminID {
			return
		}
		days, ok := validateNumber(bot, chatID, text, 1, 9999, "Durasi")
		if !ok {
			return
		}
		username := tempUserData[userID]["username"]
		renewUser(bot, chatID, username, days, config)
		resetState(userID)
	}
}

// ==========================================
// Admin & User Features
// ==========================================

func startCreateUser(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "create_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "👤 Masukkan Password Baru:")
}

func startAdminCreateUser(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "admin_create_password"
	tempUserData[userID] = make(map[string]string)
	sendMessage(bot, chatID, "👤 [Admin] Masukkan Password Baru:")
}

func startRenewUser(bot *tgbotapi.BotAPI, chatID int64, userID int64, data string) {
	username := strings.TrimPrefix(data, "select_renew:")
	tempUserData[userID] = map[string]string{"username": username}
	userStates[userID] = "admin_renew_days"
	sendMessage(bot, chatID, fmt.Sprintf("🔄 [Admin] Renew %s\n⏳ Masukkan Tambahan Durasi (hari):", username))
}

func confirmDeleteUser(bot *tgbotapi.BotAPI, chatID int64, data string) {
	username := strings.TrimPrefix(data, "select_delete:")
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("❓ [Admin] Yakin hapus user `%s`?", username))
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("✅ Ya", "confirm_delete:"+username),
			tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func createUser(bot *tgbotapi.BotAPI, chatID int64, password string, days int, config *BotConfig) {
	res, err := apiCall("POST", "/user/create", map[string]interface{}{
		"password": password,
		"days":     days,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
	}
}

func renewUser(bot *tgbotapi.BotAPI, chatID int64, username string, days int, config *BotConfig) {
	res, err := apiCall("POST", "/user/renew", map[string]interface{}{
		"password": username,
		"days":     days,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		sendAccountInfo(bot, chatID, data, config)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal renew: %s", res["message"]))
		showAdminPanel(bot, chatID)
	}
}

func deleteUser(bot *tgbotapi.BotAPI, chatID int64, username string, config *BotConfig) {
	res, err := apiCall("POST", "/user/delete", map[string]interface{}{
		"password": username,
	})

	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		sendMessage(bot, chatID, "✅ User berhasil dihapus.")
		showAdminPanel(bot, chatID)
	} else {
		replyError(bot, chatID, fmt.Sprintf("Gagal: %s", res["message"]))
		showAdminPanel(bot, chatID)
	}
}

func listUsers(bot *tgbotapi.BotAPI, chatID int64) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		users := res["data"].([]interface{})
		if len(users) == 0 {
			sendMessage(bot, chatID, "📂 Tidak ada user.")
			return
		}

		msg := "📋 *List Passwords*\n"
		for _, u := range users {
			user := u.(map[string]interface{})
			status := "🟢"
			if user["status"] == "Expired" {
				status = "🔴"
			}
			msg += fmt.Sprintf("\n%s `%s` (%s)", status, user["password"], user["expired"])
		}

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		sendAndTrack(bot, reply)
	} else {
		replyError(bot, chatID, "Gagal ambil data.")
	}
}

func showAdminPanel(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "🛠️ *Admin Panel*\nPilih menu:")
	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("👤 Create Password", "admin_create"),
			tgbotapi.NewInlineKeyboardButtonData("🗑️ Delete Password", "admin_delete"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Renew Password", "admin_renew"),
			tgbotapi.NewInlineKeyboardButtonData("📋 List Passwords", "admin_list"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Backup Data", "menu_backup_action"),
			tgbotapi.NewInlineKeyboardButtonData("⬆️ Restore Data", "menu_restore_action"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	sendAndTrack(bot, msg)
}

func getUsers() ([]UserData, error) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		return nil, err
	}
	if res["success"] != true {
		return nil, fmt.Errorf("failed")
	}
	var users []UserData
	dataBytes, _ := json.Marshal(res["data"])
	json.Unmarshal(dataBytes, &users)
	return users, nil
}

func showUserSelection(bot *tgbotapi.BotAPI, chatID int64, page int, action string) {
	users, err := getUsers()
	if err != nil {
		replyError(bot, chatID, "Gagal ambil data.")
		return
	}
	if len(users) == 0 {
		sendMessage(bot, chatID, "📂 Tidak ada user.")
		return
	}

	perPage := 10
	totalPages := (len(users) + perPage - 1) / perPage
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * perPage
	end := start + perPage
	if end > len(users) {
		end = len(users)
	}

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, u := range users[start:end] {
		label := fmt.Sprintf("%s (%s)", u.Password, u.Status)
		if u.Status == "Expired" {
			label = fmt.Sprintf("🔴 %s", label)
		} else {
			label = fmt.Sprintf("🟢 %s", label)
		}
		data := fmt.Sprintf("select_%s:%s", action, u.Password)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(label, data),
		))
	}

	var navRow []tgbotapi.InlineKeyboardButton
	if page > 1 {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("⬅️ Prev", fmt.Sprintf("page_%s:%d", action, page-1)))
	}
	if page < totalPages {
		navRow = append(navRow, tgbotapi.NewInlineKeyboardButtonData("Next ➡️", fmt.Sprintf("page_%s:%d", action, page+1)))
	}
	if len(navRow) > 0 {
		rows = append(rows, navRow)
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("❌ Batal", "cancel")))

	title := "Delete"
	if action == "renew" {
		title = "Renew"
	}
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("📋 [Admin] Pilih user untuk %s (Hal %d/%d):", title, page, totalPages))
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	sendAndTrack(bot, msg)
}

func handlePagination(bot *tgbotapi.BotAPI, chatID int64, data string) {
	parts := strings.Split(data, ":")
	action := parts[0][5:]
	page, _ := strconv.Atoi(parts[1])
	showUserSelection(bot, chatID, page, action)
}

// ==========================================
// UI & Helpers
// ==========================================

func showMainMenu(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	ipInfo, _ := getIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "(Not Configured)"
	}

	msgText := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    STORE ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\n • Domain   : %s\n • City     : %s\n • ISP      : %s\n • Harga    : Rp %d / Hari\n━━━━━━━━━━━━━━━━━━━━━\n```\nPilih menu:", domain, ipInfo.City, ipInfo.Isp, config.DailyPrice)

	msg := tgbotapi.NewMessage(chatID, msgText)
	msg.ParseMode = "Markdown"

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛒 Beli Akun Premium", "menu_create"),
			tgbotapi.NewInlineKeyboardButtonData("📊 System Info", "menu_info"),
		),
	)

	if chatID == config.AdminID {
		keyboard.InlineKeyboard = append(keyboard.InlineKeyboard, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🛠️ Admin Panel", "menu_admin"),
		))
	}

	msg.ReplyMarkup = keyboard
	sendAndTrack(bot, msg)
}

func sendAccountInfo(bot *tgbotapi.BotAPI, chatID int64, data map[string]interface{}, config *BotConfig) {
	ipInfo, _ := getIpInfo()
	domain := config.Domain
	if domain == "" {
		domain = "(Not Configured)"
	}

	msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n  PREMIUM ACCOUNT\n━━━━━━━━━━━━━━━━━━━━━\nPassword   : %s\nCITY       : %s\nISP        : %s\nDomain     : %s\nExpired On : %s\n━━━━━━━━━━━━━━━━━━━━━\n```",
		data["password"], ipInfo.City, ipInfo.Isp, domain, data["expired"])

	reply := tgbotapi.NewMessage(chatID, msg)
	reply.ParseMode = "Markdown"
	deleteLastMessage(bot, chatID)
	bot.Send(reply)
	showMainMenu(bot, chatID, config)
}

func cancelOperation(bot *tgbotapi.BotAPI, chatID int64, userID int64, config *BotConfig) {
	resetState(userID)
	if userID == config.AdminID {
		showAdminPanel(bot, chatID)
	} else {
		showMainMenu(bot, chatID, config)
	}
}

func validatePassword(bot *tgbotapi.BotAPI, chatID int64, text string) bool {
	if len(text) < 3 || len(text) > 20 {
		sendMessage(bot, chatID, "❌ Password 3-20 karakter. Coba lagi:")
		return false
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(text) {
		sendMessage(bot, chatID, "❌ Hanya huruf, angka, - dan _. Coba lagi:")
		return false
	}
	return true
}

func validateNumber(bot *tgbotapi.BotAPI, chatID int64, text string, min, max int, field string) (int, bool) {
	val, err := strconv.Atoi(text)
	if err != nil || val < min || val > max {
		sendMessage(bot, chatID, fmt.Sprintf("❌ %s harus %d-%d. Coba lagi:", field, min, max))
		return 0, false
	}
	return val, true
}

func resetState(userID int64) {
	delete(userStates, userID)
	delete(tempUserData, userID)
}

func sendMessage(bot *tgbotapi.BotAPI, chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	sendAndTrack(bot, msg)
}

func replyError(bot *tgbotapi.BotAPI, chatID int64, text string) {
	sendMessage(bot, chatID, "❌ "+text)
}

func sendAndTrack(bot *tgbotapi.BotAPI, msg tgbotapi.MessageConfig) {
	deleteLastMessage(bot, msg.ChatID)
	sent, err := bot.Send(msg)
	if err == nil {
		lastMessageIDs[msg.ChatID] = sent.MessageID
	}
}

func deleteLastMessage(bot *tgbotapi.BotAPI, chatID int64) {
	if id, ok := lastMessageIDs[chatID]; ok {
		bot.Request(tgbotapi.NewDeleteMessage(chatID, id))
		delete(lastMessageIDs, chatID)
	}
}

func systemInfo(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	res, err := apiCall("GET", "/info", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}

	if res["success"] == true {
		data := res["data"].(map[string]interface{})
		ipInfo, _ := getIpInfo()

		msg := fmt.Sprintf("```\n━━━━━━━━━━━━━━━━━━━━━\n    INFO ZIVPN UDP\n━━━━━━━━━━━━━━━━━━━━━\nDomain     : %s\nIP Public  : %s\nPort       : %s\nService    : %s\nCity       : %s\nISP        : %s\n━━━━━━━━━━━━━━━━━━━━━\n```",
			config.Domain, data["public_ip"], data["port"], data["service"], ipInfo.City, ipInfo.Isp)

		reply := tgbotapi.NewMessage(chatID, msg)
		reply.ParseMode = "Markdown"
		deleteLastMessage(bot, chatID)
		bot.Send(reply)
		showMainMenu(bot, chatID, config)
	} else {
		replyError(bot, chatID, "Gagal ambil info.")
	}
}

func performBackup(bot *tgbotapi.BotAPI, chatID int64) {
	sendMessage(bot, chatID, "⏳ Sedang membuat backup...")

	files := []string{
		"/etc/zivpn/config.json",
		"/etc/zivpn/users.json",
		"/etc/zivpn/domain",
	}

	buf := new(bytes.Buffer)
	zipWriter := zip.NewWriter(buf)

	for _, file := range files {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		}

		f, err := os.Open(file)
		if err != nil {
			continue
		}
		defer f.Close()

		w, err := zipWriter.Create(filepath.Base(file))
		if err != nil {
			continue
		}

		if _, err := io.Copy(w, f); err != nil {
			continue
		}
	}

	zipWriter.Close()

	fileName := fmt.Sprintf("zivpn-backup-%s.zip", time.Now().Format("20060102-150405"))
	tmpFile := "/tmp/" + fileName
	if err := ioutil.WriteFile(tmpFile, buf.Bytes(), 0644); err != nil {
		replyError(bot, chatID, "Gagal membuat file backup.")
		return
	}
	defer os.Remove(tmpFile)

	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(tmpFile))
	doc.Caption = "✅ Backup Data ZiVPN"

	deleteLastMessage(bot, chatID)
	bot.Send(doc)
}

func startRestore(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "waiting_restore_file"
	sendMessage(bot, chatID, "⬆️ *Restore Data*\n\nKirim file ZIP backup.\n⚠️ Data saat ini akan ditimpa!")
}

func processRestoreFile(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	chatID := msg.Chat.ID
	userID := msg.From.ID

	resetState(userID)
	sendMessage(bot, chatID, "⏳ Memproses file...")

	fileID := msg.Document.FileID
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		replyError(bot, chatID, "Gagal unduh file.")
		return
	}

	fileUrl := file.Link(config.BotToken)
	resp, err := http.Get(fileUrl)
	if err != nil {
		replyError(bot, chatID, "Gagal unduh content.")
		return
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		replyError(bot, chatID, "Gagal baca file.")
		return
	}

	zipReader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		replyError(bot, chatID, "File bukan ZIP valid.")
		return
	}

	validFiles := map[string]bool{
		"config.json":     true,
		"users.json":      true,
		"bot-config.json": true,
		"domain":          true,
		"apikey":          true,
	}

	for _, f := range zipReader.File {
		if !validFiles[f.Name] {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			continue
		}
		defer rc.Close()

		dstPath := filepath.Join("/etc/zivpn", f.Name)
		dst, err := os.Create(dstPath)
		if err != nil {
			continue
		}
		defer dst.Close()

		io.Copy(dst, rc)
	}

	exec.Command("systemctl", "restart", "zivpn").Run()
	exec.Command("systemctl", "restart", "zivpn-api").Run()

	msgSuccess := tgbotapi.NewMessage(chatID, "✅ Restore berhasil! Service direstart.")
	bot.Send(msgSuccess)

	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("systemctl", "restart", "zivpn-bot").Run()
	}()

	showMainMenu(bot, chatID, config)
}

func loadConfig() (BotConfig, error) {
	var config BotConfig
	file, err := ioutil.ReadFile(BotConfigFile)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(file, &config)

	if config.Domain == "" {
		if domainBytes, err := ioutil.ReadFile(DomainFile); err == nil {
			config.Domain = strings.TrimSpace(string(domainBytes))
		}
	}

	return config, err
}

func apiCall(method, endpoint string, payload interface{}) (map[string]interface{}, error) {
	var reqBody []byte
	var err error

	if payload != nil {
		reqBody, err = json.Marshal(payload)
		if err != nil {
			return nil, err
		}
	}

	client := &http.Client{}
	req, err := http.NewRequest(method, ApiUrl+endpoint, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", ApiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	var result map[string]interface{}
	json.Unmarshal(body, &result)

	return result, nil
}

func getIpInfo() (IpInfo, error) {
	resp, err := http.Get("http://ip-api.com/json/")
	if err != nil {
		return IpInfo{}, err
	}
	defer resp.Body.Close()

	var info IpInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return IpInfo{}, err
	}
	return info, nil
}
