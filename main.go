package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/logger"
	"github.com/gofiber/websocket/v2"
)

type Message struct {
	ID       int       `json:"id"`
	Filename string    `json:"filename"`
	Size     int64     `json:"size"`
	MimeType string    `json:"mimetype"`
	Data     []byte    `json:"-"` // don't serialize the actual data
	Created  time.Time `json:"created"`
}

type MessageResponse struct {
	ID       int       `json:"id"`
	Filename string    `json:"filename"`
	Size     int64     `json:"size"`
	Created  time.Time `json:"created"`
}

type WSMessage struct {
	Type     string `json:"type"`
	Filename string `json:"filename,omitempty"`
	ID       int    `json:"id,omitempty"`
}

type Server struct {
	messages     []Message
	nextID       int
	mutex        sync.RWMutex
	clients      map[*websocket.Conn]struct{}
	clientsMutex sync.RWMutex
}

var acceptedFormats = []string{
	"mp3", "mp4", "mkv", "png", "jpeg", "jpg", "pdf", "webm", "ogg", "txt", "json", "yaml", "yml", "zip",
}

const (
	oneMB       = 1024 * 1024
	maxFileSize = 1024 * oneMB // 1 GB
)

var (
	port = flag.Int("port", 3000, "port to run the server on")
)

func main() {
	server := &Server{
		messages: make([]Message, 0),
		nextID:   1,
		clients:  make(map[*websocket.Conn]struct{}),
	}

	app := fiber.New(fiber.Config{
		BodyLimit: maxFileSize + 1024, // slightly larger to handle multipart overhead
	})

	// mw
	app.Use(logger.New())
	app.Use(cors.New(cors.Config{
		AllowOrigins: "*",
		AllowMethods: "GET,POST,HEAD,PUT,DELETE,PATCH,OPTIONS",
		AllowHeaders: "*",
	}))

	app.Static("/", "./")

	// API
	app.Post("/upload", server.handleUpload)
	app.Post("/record", server.handleRecord)
	app.Get("/messages", server.handleGetMessages)
	app.Get("/message/:id", server.handleGetMessage)
	app.Get("/formats", server.handleGetFormats)

	// ws endpoint
	app.Use("/ws", func(c *fiber.Ctx) error {
		if websocket.IsWebSocketUpgrade(c) {
			c.Locals("allowed", true)
			return c.Next()
		}
		return fiber.ErrUpgradeRequired
	})
	app.Get("/ws", websocket.New(server.handleWebSocket))

	log.Printf("🚀 server starting on http://localhost:%d", *port)
	log.Printf("📁 accepted formats: %s", strings.Join(acceptedFormats, ", "))
	log.Printf("📏 max file size: %s", toMB(maxFileSize))

	_ = app.Listen(fmt.Sprintf(":%d", *port)) // TODO: check error properly
}

func validateFileFormat(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext != "" && ext[0] == '.' {
		ext = ext[1:] // remove the dot
	}
	return slices.Contains(acceptedFormats, ext)
}

func (s *Server) handleUpload(c *fiber.Ctx) error {
	form, err := c.MultipartForm()
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{
			"error": "parse multipart form",
		})
	}

	files := form.File["file"]
	if len(files) == 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{
			"error": "no file provided",
		})
	}

	uploaded := make([]MessageResponse, 0)

	for _, file := range files {
		if !validateFileFormat(file.Filename) {
			continue // skip unsupported files
		}

		// check file size
		if file.Size > maxFileSize {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{
				"error": fmt.Sprintf("file %q exceeds maximum size of %d MB", file.Filename, maxFileSize/oneMB),
			})
		}

		// read file data
		src, err := file.Open()
		if err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{
				"error": "open uploaded file",
			})
		}

		data, err := io.ReadAll(src)
		_ = src.Close()
		if err != nil {
			return c.Status(http.StatusInternalServerError).JSON(fiber.Map{
				"error": "read uploaded file",
			})
		}

		// Store in memory
		message := s.storeMessage(file.Filename, data, file.Header.Get("Content-Type"))
		uploaded = append(uploaded, MessageResponse{
			ID:       message.ID,
			Filename: message.Filename,
			Size:     message.Size,
			Created:  message.Created,
		})

		// notify WebSocket clients
		s.broadcastNewMessage(message.Filename, message.ID)
	}

	return c.JSON(fiber.Map{
		"success":  true,
		"uploaded": uploaded,
	})
}

func (s *Server) handleRecord(c *fiber.Ctx) error {
	// This endpoint can be used the same as /upload
	// but we keep it separate for clarity and potential future differences
	return s.handleUpload(c)
}

func (s *Server) handleGetFormats(c *fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"formats":        acceptedFormats,
		"max_size_bytes": maxFileSize,
		"max_size_mb":    maxFileSize / (1024 * 1024),
	})
}

func (s *Server) handleGetMessages(c *fiber.Ctx) error {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	responses := make([]MessageResponse, len(s.messages))
	for i, msg := range s.messages {
		responses[i] = MessageResponse{
			ID:       msg.ID,
			Filename: msg.Filename,
			Size:     msg.Size,
			Created:  msg.Created,
		}
	}

	return c.JSON(responses)
}

func (s *Server) handleGetMessage(c *fiber.Ctx) error {
	idStr := c.Params("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{
			"error": "invalid message_id",
		})
	}

	s.mutex.RLock()
	defer s.mutex.RUnlock()

	// Find the message
	var message *Message
	for i := range s.messages {
		if s.messages[i].ID == id {
			message = &s.messages[i]
			break
		}
	}

	if message == nil {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{
			"error": "message not found",
		})
	}

	// Set appropriate headers
	c.Set("Content-Type", message.MimeType)
	c.Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", message.Filename))
	c.Set("Content-Length", strconv.FormatInt(message.Size, 10))

	// For audio files, also allow inline playing
	ext := strings.ToLower(filepath.Ext(message.Filename))
	if ext == ".mp3" || ext == ".webm" || ext == ".ogg" {
		c.Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", message.Filename))
	}

	return c.Send(message.Data)
}

func (s *Server) handleWebSocket(c *websocket.Conn) {
	// Register client
	s.clientsMutex.Lock()
	s.clients[c] = struct{}{}
	s.clientsMutex.Unlock()

	log.Printf("🔌 ws client connected, total clients: %d", len(s.clients))

	// Send welcome message
	welcomeMsg := WSMessage{Type: "connected"}
	_ = c.WriteJSON(welcomeMsg)

	// Handle disconnection
	defer func() {
		s.clientsMutex.Lock()
		delete(s.clients, c)
		s.clientsMutex.Unlock()
		log.Printf("🔌 ws client disconnected, total clients: %d", len(s.clients))
	}()

	// keep connection alive and handle incoming messages
	for {
		var msg WSMessage
		err := c.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("ws error: %v", err)
			}
			break
		}
		// Echo back or handle specific message types if needed
		// For now, we just keep the connection alive
	}
}

func (s *Server) storeMessage(filename string, data []byte, mimeType string) Message {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	// If mimeType is empty, try to guess from extension
	if mimeType == "" {
		ext := strings.ToLower(filepath.Ext(filename))
		switch ext {
		case ".mp3":
			mimeType = "audio/mpeg"
		case ".mp4":
			mimeType = "video/mp4"
		case ".mkv":
			mimeType = "video/x-matroska"
		case ".webm":
			mimeType = "audio/webm"
		case ".ogg":
			mimeType = "audio/ogg"
		case ".png":
			mimeType = "image/png"
		case ".jpg", ".jpeg":
			mimeType = "image/jpeg"
		case ".pdf":
			mimeType = "application/pdf"
		case ".txt":
			mimeType = "text/plain"
		case ".html":
			mimeType = "text/html"
		case ".css":
			mimeType = "text/css"
		case ".js":
			mimeType = "application/javascript"
		case ".json":
			mimeType = "application/json"
		case ".yaml", ".yml":
			mimeType = "text/yaml"
		default:
			mimeType = "application/octet-stream"
		}
	}

	message := Message{
		ID:       s.nextID,
		Filename: filename,
		Size:     int64(len(data)),
		MimeType: mimeType,
		Data:     data,
		Created:  time.Now(),
	}

	s.messages = append(s.messages, message)
	s.nextID++

	log.Printf("📁 stored file: %s (ID: %d, Size: %s)", filename, message.ID, toMB(len(data)))
	return message
}

func (s *Server) broadcastNewMessage(filename string, id int) {
	s.clientsMutex.RLock()
	defer s.clientsMutex.RUnlock()

	if len(s.clients) == 0 {
		return
	}

	message := WSMessage{
		Type:     "new_message",
		Filename: filename,
		ID:       id,
	}

	// Send to all connected clients
	for client := range s.clients {
		err := client.WriteJSON(message)
		if err != nil {
			log.Printf("send ws message: %v", err)
			// Remove broken connection
			delete(s.clients, client)
		}
	}

	log.Printf("📡 broadcast new message to %d clients: %s", len(s.clients), filename)
}

func toMB(size int) string {
	if size < oneMB {
		return fmt.Sprintf("%d bytes", size)
	}
	return fmt.Sprintf("%.2f MB", float64(size)/oneMB)
}
