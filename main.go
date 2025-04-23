package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"

	"time"

	"golang.org/x/time/rate"
	_ "modernc.org/sqlite"
)

// ---------------------- CONFIG ----------------------

type Config struct {
	Palworld struct {
		Host     string `json:"host"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"palworld"`
}

func loadConfig(path string) (*Config, error) {
	data, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var config Config
	err = json.Unmarshal(data, &config)
	return &config, err
}

// ---------------------- PALWORLD STRUCTS ----------------------

type PalworldMetrics struct {
	ServerFPS        int     `json:"serverfps"`
	CurrentPlayerNum int     `json:"currentplayernum"`
	ServerFrameTime  float64 `json:"serverframetime"`
	MaxPlayerNum     int     `json:"maxplayernum"`
	Uptime           int     `json:"uptime"`
	Days             int     `json:"days"`
}

type PlayerList struct {
	Players []Player `json:"players"`
}

type Player struct {
	Name        string  `json:"name"`
	AccountName string  `json:"accountName"`
	PlayerID    string  `json:"playerId"`
	UserID      string  `json:"userId"`
	IP          string  `json:"ip"`
	Ping        float64 `json:"ping"`
	LocationX   float64 `json:"location_x"`
	LocationY   float64 `json:"location_y"`
	Level       int     `json:"level"`
}

// ---------------------- GLOBALE VARS ----------------------

var globalLimiter = rate.NewLimiter(rate.Every(1*time.Second), 1) // 1 msg/sec max globaal
// Cooldown per user JID per command
var userCooldowns = make(map[string]map[string]time.Time)
var notifiedCooldowns = make(map[string]map[string]bool)

// ---------------------- MAIN BOT LOGICA ----------------------

func main() {
	logger := waLog.Stdout("INFO", "BOT", false)

	config, err := loadConfig("config.json")
	if err != nil {
		panic("config.json niet gevonden of ongeldig")
	}

	store, err := sqlstore.New("sqlite", "file:bot.db?_pragma=foreign_keys(1)", logger)
	if err != nil {
		panic(err)
	}
	device, err := store.GetFirstDevice()
	if err != nil {
		panic(err)
	}
	client := whatsmeow.NewClient(device, logger)

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			handleMessage(v, client, config)
		}
	})

	if client.Store.ID == nil {
		qrChan, _ := client.GetQRChannel(context.Background())
		go func() {
			for evt := range qrChan {
				if evt.Event == "code" {
					fmt.Println("Scan QR:", evt.Code)
				}
			}
		}()
		_ = client.Connect()
	} else {
		_ = client.Connect()
	}

	fmt.Println("Bot actief - druk op Ctrl+C om te stoppen.")
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	<-ch
	client.Disconnect()
}

// ---------------------- HANDLE MESSAGE ----------------------

func handleMessage(v *events.Message, client *whatsmeow.Client, config *Config) {
	var msg string

	switch {
	case v.Message.GetConversation() != "":
		msg = v.Message.GetConversation()
	case v.Message.GetExtendedTextMessage() != nil:
		msg = v.Message.GetExtendedTextMessage().GetText()
	default:
		msg = "[onbekend berichttype]"
	}
	switch {
	case strings.TrimSpace(msg) == "/metrics":
		metrics, err := fetchMetrics(config)
		if err != nil {
			sendText(client, v, "❌ Fout bij ophalen van serverstatus.")
			return
		}

		response := fmt.Sprintf(
			"📊 *Palworld Serverstatus*\n\n"+
				"👥 Spelers: %d/%d\n"+
				"🎮 FPS: %d\n"+
				"🕒 Uptime: %d seconden\n"+
				"📅 In-game dagen: %d\n"+
				"⏱️ Frame time: %.2f ms",
			metrics.CurrentPlayerNum,
			metrics.MaxPlayerNum,
			metrics.ServerFPS,
			metrics.Uptime,
			metrics.Days,
			metrics.ServerFrameTime,
		)
		sendText(client, v, response)
	case strings.TrimSpace(msg) == "/players":
		players, err := fetchPlayers(config)
		if err != nil {
			sendText(client, v, "❌ Fout bij ophalen van spelerslijst.")
			return
		}

		if len(players.Players) == 0 {
			sendText(client, v, "🕵️ Geen spelers online.")
			return
		}

		var list strings.Builder
		list.WriteString("🧑‍💻 *Online spelers:*\n\n")
		for i, p := range players.Players {
			list.WriteString(fmt.Sprintf(
				"👤 *%s* (%s)\n"+
					"🆔 `%s`\n"+
					"📍 `X=%.1f, Y=%.1f`\n"+
					"📶 Ping: %.2f ms\n"+
					"🧬 Level: %d",
				p.Name, p.AccountName,
				p.UserID,
				p.LocationX, p.LocationY,
				p.Ping,
				p.Level,
			))
			// Alleen newline toevoegen als het NIET de laatste of tiende is
			if i < len(players.Players)-1 && i < 9 {
				list.WriteString("\n\n")
			}
			if i >= 9 {
				list.WriteString("... (max 10 weergegeven)")
				break
			}
		}
		sendText(client, v, list.String())
	}
}

// ---------------------- SEND MESSAGE ----------------------

//	func sendText(client *whatsmeow.Client, v *events.Message, text string) {
//		msg := &waE2E.Message{
//			Conversation: proto.String(text),
//		}
//		_, _ = client.SendMessage(context.Background(), v.Info.Chat, msg)
//	}
func sendText(client *whatsmeow.Client, v *events.Message, text string) {
	jid := v.Info.Sender.String()

	if !globalLimiter.Allow() {
		fmt.Println("⏳ Rate limit (globaal)")
		return
	}

	cmd := extractCommand(text) // optioneel, voor nettere cooldown per commando

	if cooldown, notify := tooSoon(jid, cmd); cooldown {
		if notify {
			// Eenmalige melding
			msg := &waE2E.Message{
				Conversation: proto.String("⏱️ Rustig aan! Je moet even wachten tot je dit commando weer kan gebruiken."),
			}
			_, _ = client.SendMessage(context.Background(), v.Info.Chat, msg)
		}
		return
	}

	// Normale verzending
	msg := &waE2E.Message{
		Conversation: proto.String(text),
	}
	_, _ = client.SendMessage(context.Background(), v.Info.Chat, msg)
}

// ---------------------- FETCH INFO VIA API ----------------------

func fetchMetrics(config *Config) (*PalworldMetrics, error) {
	url := config.Palworld.Host + "/v1/api/metrics"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(config.Palworld.Username, config.Palworld.Password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d van server", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data PalworldMetrics
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

func fetchPlayers(config *Config) (*PlayerList, error) {
	url := config.Palworld.Host + "/v1/api/players"

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(config.Palworld.Username, config.Palworld.Password)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d van server", resp.StatusCode)
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var data PlayerList
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	return &data, nil
}

// ---------------------- COOLDOWN RATE LIMIT ----------------------

func tooSoon(jid, cmd string) (bool, bool) {
	now := time.Now()

	// Init maps
	if _, ok := userCooldowns[jid]; !ok {
		userCooldowns[jid] = make(map[string]time.Time)
	}
	if _, ok := notifiedCooldowns[jid]; !ok {
		notifiedCooldowns[jid] = make(map[string]bool)
	}

	lastUsed, exists := userCooldowns[jid][cmd]
	if exists && now.Sub(lastUsed) < 30*time.Second {
		alreadyNotified := notifiedCooldowns[jid][cmd]
		if !alreadyNotified {
			notifiedCooldowns[jid][cmd] = true
			return true, true // cooldown actief, en eerste keer
		}
		return true, false // cooldown actief, al gemeld
	}

	// Cooldown voorbij, reset melding
	userCooldowns[jid][cmd] = now
	notifiedCooldowns[jid][cmd] = false
	return false, false
}

func extractCommand(text string) string {
	text = strings.TrimSpace(text)

	if strings.HasPrefix(text, "/") {
		// Alleen het commando zelf gebruiken (bijv. "/players")
		parts := strings.SplitN(text, " ", 2)
		return parts[0]
	}

	// Geen commando, geef de eerste 20 tekens als fallback (optioneel)
	if len(text) > 20 {
		return text[:20]
	}
	return text
}
