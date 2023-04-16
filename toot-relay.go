package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sideshow/apns2"
	"github.com/sideshow/apns2/payload"
	"github.com/sideshow/apns2/token"
)

var (
	client *apns2.Client
)

func main() {
	p8PrivateKey := env("P8_PRIVATE_KEY", "")
	p8KeyID := env("P8_KEY_ID", "")
	p8TeamID := env("P8_TEAM_ID", "")

	authKey, err := token.AuthKeyFromBytes([]byte(p8PrivateKey))
	if err != nil {
		log.Fatal("token error:", err)
	}

	token := &token.Token{
		AuthKey: authKey,
		// KeyID from developer account (Certificates, Identifiers & Profiles -> Keys)
		KeyID: p8KeyID,
		// TeamID from developer account (View Account -> Membership)
		TeamID: p8TeamID,
	}
	isProduction := env("APNS_ENVIRONMENT", "")
	if isProduction == "PRODUCTION" {
		client = apns2.NewTokenClient(token).Production()
	} else {
		client = apns2.NewTokenClient(token).Development()
	}

	http.HandleFunc("/relay-to/", handler)

	http.HandleFunc("/ping", func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, "pong")
	})
	port := env("PORT", "")
	http.ListenAndServe(":"+port, nil)
}

func handler(writer http.ResponseWriter, request *http.Request) {
	components := strings.Split(request.URL.Path, "/")

	if len(components) < 3 {
		writer.WriteHeader(500)
		fmt.Fprintln(writer, "Invalid URL path:", request.URL.Path)
		log.Println("Invalid URL path:", request.URL.Path)
		return
	}

	notification := &apns2.Notification{}
	notification.DeviceToken = components[2]

	buffer := new(bytes.Buffer)
	buffer.ReadFrom(request.Body)
	encodedString := encode85(buffer.Bytes())
	payload := payload.NewPayload().Alert("🎺").MutableContent().ContentAvailable().Custom("p", encodedString)

	if len(components) > 3 {
		payload.Custom("x", strings.Join(components[3:], "/"))
	}

	notification.Payload = payload
	notification.Topic = "dev.noppe.snowfox"

	switch request.Header.Get("Content-Encoding") {
	case "aesgcm":
		if publicKey, err := encodedValue(request.Header, "Crypto-Key", "dh"); err == nil {
			payload.Custom("k", publicKey)
		} else {
			writer.WriteHeader(500)
			fmt.Fprintln(writer, "Error retrieving public key:", err)
			log.Println("Error retrieving public key:", err)
			return
		}

		if salt, err := encodedValue(request.Header, "Encryption", "salt"); err == nil {
			payload.Custom("s", salt)
		} else {
			writer.WriteHeader(500)
			fmt.Fprintln(writer, "Error retrieving salt:", err)
			log.Println("Error retrieving salt:", err)
			return
		}
	//case "aes128gcm": // No further headers needed. However, not implemented on client side so return 415.
	default:
		writer.WriteHeader(415)
		fmt.Fprintln(writer, "Unsupported Content-Encoding:", request.Header.Get("Content-Encoding"))
		log.Println("Unsupported Content-Encoding:", request.Header.Get("Content-Encoding"))
		return
	}

	if seconds := request.Header.Get("TTL"); seconds != "" {
		if ttl, err := strconv.Atoi(seconds); err == nil {
			notification.Expiration = time.Now().Add(time.Duration(ttl) * time.Second)
		}
	}

	if topic := request.Header.Get("Topic"); topic != "" {
		notification.CollapseID = topic
	}

	switch request.Header.Get("Urgency") {
	case "very-low", "low":
		notification.Priority = apns2.PriorityLow
	default:
		notification.Priority = apns2.PriorityHigh
	}

	res, err := client.Push(notification)
	if err != nil {
		writer.WriteHeader(500)
		fmt.Fprintln(writer, "Push error:", err)
		log.Println("Push error:", err)
		return
	}

	if res.Sent() {
		writer.Header().Add("Location", fmt.Sprintf("https://not-supported/%v", res.ApnsID))
		writer.WriteHeader(201)
		log.Printf("Sent notification to %s -> %v %v %v", notification.DeviceToken, res.StatusCode, res.ApnsID, res.Reason)
		log.Println("Expiration:", notification.Expiration)
		log.Println("Priority:", notification.Priority)
		log.Println("CollapseID:", notification.CollapseID)
	} else {
		writer.WriteHeader(res.StatusCode)
		fmt.Fprintln(writer, res.Reason)
		log.Printf("Failed to send: %v %v %v\n", res.StatusCode, res.ApnsID, res.Reason)
	}
}

func env(name, defaultValue string) string {
	if value, isPresent := os.LookupEnv(name); isPresent {
		return value
	} else {
		return defaultValue
	}
}

func encodedValue(header http.Header, name, key string) (string, error) {
	keyValues := parseKeyValues(header.Get(name))
	value, exists := keyValues[key]
	if !exists {
		return "", errors.New(fmt.Sprintf("Value %s not found in header %s", key, name))
	}

	bytes, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}

	return encode85(bytes), nil
}

func parseKeyValues(values string) map[string]string {
	f := func(c rune) bool {
		return c == ';'
	}

	entries := strings.FieldsFunc(values, f)

	m := make(map[string]string)
	for _, entry := range entries {
		parts := strings.Split(entry, "=")
		m[parts[0]] = parts[1]
	}

	return m
}

var z85digits = []byte("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ.-:+=^!/*?&<>()[]{}@%$#")

func encode85(bytes []byte) string {
	numBlocks := len(bytes) / 4
	suffixLength := len(bytes) % 4

	encodedLength := numBlocks * 5
	if suffixLength != 0 {
		encodedLength += suffixLength + 1
	}

	encodedBytes := make([]byte, encodedLength)

	src := bytes
	dest := encodedBytes
	for block := 0; block < numBlocks; block++ {
		value := binary.BigEndian.Uint32(src)

		for i := 0; i < 5; i++ {
			dest[4-i] = z85digits[value%85]
			value /= 85
		}

		src = src[4:]
		dest = dest[5:]
	}

	if suffixLength != 0 {
		value := 0

		for i := 0; i < suffixLength; i++ {
			value *= 256
			value |= int(src[i])
		}

		for i := 0; i < suffixLength+1; i++ {
			dest[suffixLength-i] = z85digits[value%85]
			value /= 85
		}
	}

	return string(encodedBytes)
}
