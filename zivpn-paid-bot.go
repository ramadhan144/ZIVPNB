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
	AdminID        int64  `json:"admin_id"`
	Mode           string `json:"mode"`
	Domain         string `json:"domain"`
	PakasirSlug    string `json:"pakasir_slug"`
	PakasirApiKey  string `json:"pakasir_api_key"`
	DailyPrice     int    `json:"daily_price"`
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

	// Load API Port
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

	// Start Payment Checker
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
// Telegram Event Handlers
// ==========================================

func handleMessage(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, config *BotConfig) {
	// In Paid Bot, everyone can access, but actions are restricted/paid
	// Admin still has full control

	if state, exists := userStates[msg.From.ID]; exists {
		handleState(bot, msg, state, config)
		return
	}

	// Handle Document Upload (Restore) - Admin Only
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

	switch query.Data {
	case "menu_create":
		startCreateUser(bot, chatID, userID)
	case "menu_info":
		systemInfo(bot, chatID, config)
	case "cancel":
		cancelOperation(bot, chatID, userID, config)

	case "menu_admin":
		if userID == config.AdminID {
			showAdminMenu(bot, chatID)
		}
	case "menu_backup_action":
		if userID == config.AdminID {
			performBackup(bot, chatID)
		}
	case "menu_restore_action":
		if userID == config.AdminID {
			startRestore(bot, chatID, userID)
		}
		// === FITUR BARU ADMIN ===
	case "admin_create":
		if userID == config.AdminID {
			startAdminCreate(bot, chatID, userID)
		}
	case "admin_delete":
		if userID == config.AdminID {
			startAdminDelete(bot, chatID, userID)
		}
	case "admin_renew":
		if userID == config.AdminID {
			startAdminRenew(bot, chatID, userID)
		}
	case "admin_list":
		if userID == config.AdminID {
			listAccounts(bot, chatID, config)
		}
	}

	bot.Request(tgbotapi.NewCallback(query.ID, ""))
}

func handleState(bot *tgbotapi.BotAPI, msg *tgbotapi.Message, state string, config *BotConfig) {
	userID := msg.From.ID
	text := strings.TrimSpace(msg.Text)
	chatID := msg.Chat.ID

	// Blokir akses non-admin ke state admin
	if strings.HasPrefix(state, "admin_") && userID != config.AdminID {
		replyError(bot, chatID, "Akses ditolak.")
		resetState(userID)
		return
	}

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

		// === ADMIN CREATE ===
	case "admin_create_password":
		if !validatePassword(bot, chatID, text) {
			return
		}
		mutex.Lock()
		tempUserData[userID]["password"] = text
		mutex.Unlock()
		userStates[userID] = "admin_create_days"
		sendMessage(bot, chatID, "⏳ Masukkan Durasi (hari):")

	case "admin_create_days":
		days, ok := validateNumber(bot, chatID, text, 1, 365, "Durasi")
		if !ok {
			return
		}
		password := tempUserData[userID]["password"]
		createUser(bot, chatID, password, days, config)
		resetState(userID)
		delete(tempUserData, userID)

		// === ADMIN DELETE ===
	case "admin_delete_password":
		deleteAccount(bot, chatID, text)
		resetState(userID)
		delete(tempUserData, userID)

		// === ADMIN RENEW ===
	case "admin_renew_password":
		mutex.Lock()
		tempUserData[userID]["password"] = text
		mutex.Unlock()
		userStates[userID] = "admin_renew_days"
		sendMessage(bot, chatID, "⏳ Masukkan jumlah hari tambahan:")

	case "admin_renew_days":
		days, ok := validateNumber(bot, chatID, text, 1, 365, "Durasi tambahan")
		if !ok {
			return
		}
		password := tempUserData[userID]["password"]
		renewAccount(bot, chatID, password, days)
		resetState(userID)
		delete(tempUserData, userID)
	}
}

// ==========================================
// Admin Features
// ==========================================

func showAdminMenu(bot *tgbotapi.BotAPI, chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "🛠️ *Admin Panel*\n\nSilakan pilih menu:")
	msg.ParseMode = "Markdown"
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Create Account", "admin_create"),
			tgbotapi.NewInlineKeyboardButtonData("➖ Delete Account", "admin_delete"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔄 Renew Account", "admin_renew"),
			tgbotapi.NewInlineKeyboardButtonData("📋 List Accounts", "admin_list"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⬇️ Backup Data", "menu_backup_action"),
			tgbotapi.NewInlineKeyboardButtonData("⬆️ Restore Data", "menu_restore_action"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("❌ Kembali", "cancel"),
		),
	)
	msg.ReplyMarkup = keyboard
	sendAndTrack(bot, msg)
}

func startAdminCreate(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "admin_create_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	mutex.Unlock()
	sendMessage(bot, chatID, "➕ *Create Account Manual*\n\nMasukkan Password Baru:")
}

func startAdminDelete(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "admin_delete_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	mutex.Unlock()
	sendMessage(bot, chatID, "➖ *Delete Account*\n\nMasukkan Password yang ingin dihapus:")
}

func startAdminRenew(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "admin_renew_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	mutex.Unlock()
	sendMessage(bot, chatID, "🔄 *Renew Account*\n\nMasukkan Password yang ingin diperpanjang:")
}

func listAccounts(bot *tgbotapi.BotAPI, chatID int64, config *BotConfig) {
	res, err := apiCall("GET", "/users", nil)
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}
	if success, ok := res["success"].(bool); !ok || !success {
		replyError(bot, chatID, "Gagal mengambil daftar akun.")
		return
	}

	data, ok := res["data"].([]interface{})
	if !ok {
		data = []interface{}{}
	}

	text := "```\n━━━━━━━━━━━━━━━━━━━━━\n     DAFTAR AKUN\n━━━━━━━━━━━━━━━━━━━━━\n"
	if len(data) == 0 {
		text += "Tidak ada akun premium.\n"
	} else {
		for i, item := range data {
			u := item.(map[string]interface{})
			password := getString(u, "password")
			expired := getString(u, "expired")
			status := getString(u, "status")
			if status == "" {
				status = "active"
			}
			text += fmt.Sprintf("%d. Password: %s\n   Expired : %s\n   Status  : %s\n\n", i+1, password, expired, status)
		}
	}
	text += "━━━━━━━━━━━━━━━━━━━━━\n```"

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	deleteLastMessage(bot, chatID)
	bot.Send(msg)
	showAdminMenu(bot, chatID)
}

func deleteAccount(bot *tgbotapi.BotAPI, chatID int64, password string) {
	res, err := apiCall("POST", "/user/delete", map[string]interface{}{"password": password})
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}
	if success, _ := res["success"].(bool); success {
		sendMessage(bot, chatID, fmt.Sprintf("✅ Akun dengan password `%s` berhasil dihapus.", password))
	} else {
		msg := "Gagal menghapus akun."
		if m, ok := res["message"].(string); ok {
			msg += " " + m
		}
		replyError(bot, chatID, msg)
	}
	showAdminMenu(bot, chatID)
}

func renewAccount(bot *tgbotapi.BotAPI, chatID int64, password string, days int) {
	res, err := apiCall("POST", "/user/renew", map[string]interface{}{
		"password": password,
		"days":     days,
	})
	if err != nil {
		replyError(bot, chatID, "Error API: "+err.Error())
		return
	}
	if success, _ := res["success"].(bool); success {
		sendMessage(bot, chatID, fmt.Sprintf("✅ Akun `%s` berhasil diperpanjang %d hari.", password, days))
	} else {
		msg := "Gagal renew akun."
		if m, ok := res["message"].(string); ok {
			msg += " " + m
		}
		replyError(bot, chatID, msg)
	}
	showAdminMenu(bot, chatID)
}

func getString(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return "Unknown"
}

// ==========================================
// Existing Features (tidak diubah)
// ==========================================

// ... (semua fungsi lain tetap sama seperti kode asli Anda:
// startCreateUser, processPayment, createUser, sendAccountInfo, 
// showMainMenu, performBackup, startRestore, processRestoreFile, 
// apiCall, getIpInfo, validatePassword, validateNumber, dll.)

// Hanya fungsi kecil yang diperlukan untuk kompatibilitas ditambahkan di bawah ini jika belum ada

func startCreateUser(bot *tgbotapi.BotAPI, chatID int64, userID int64) {
	userStates[userID] = "create_password"
	mutex.Lock()
	tempUserData[userID] = make(map[string]string)
	tempUserData[userID]["chat_id"] = strconv.FormatInt(chatID, 10)
	mutex.Unlock()
	sendMessage(bot, chatID, "👤 Masukkan Password Baru:")
}

// (sisanya sama persis dengan kode Anda yang asli, hanya ditambahkan fungsi-fungsi baru di atas)
