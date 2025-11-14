package client

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MetaGLM/glm-realtime-sdk/golang/events"
	"github.com/gorilla/websocket"
)

type RealtimeClient interface {
	Connect() error
	Disconnect() error
	Send(event *events.Event) error
	SendFrameByVideo(event *events.Event) error
	FlushVideoFrames() error
	Wait()
	SetInstructions(instructions string)
}

type realtimeClient struct {
	url, apiKey string
	onReceived  func(event *events.Event) error
	conn        *websocket.Conn

	isConnected bool
	lock        sync.RWMutex
	wg          *sync.WaitGroup

	videoFrames     [][]byte
	videoFrameMutex sync.Mutex
	maxFrameCount   int
	instructions    string
}

const waitTimeout = 30 * time.Second // Define a default timeout for wait

func NewRealtimeClient(url, apiKey string, onReceived func(event *events.Event) error) *realtimeClient {
	return &realtimeClient{
		url:           url,
		apiKey:        apiKey,
		onReceived:    onReceived,
		videoFrames:   make([][]byte, 0),
		maxFrameCount: 10,
		instructions:  "请描述这个视频的内容",
	}
}

func (r *realtimeClient) Connect() error {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.isConnected {
		return nil
	}
	var header http.Header
	if r.apiKey != "" {
		header = make(http.Header)
		header.Set("Authorization", fmt.Sprintf("Bearer %s", r.apiKey))
	}
	c, rsp, err := websocket.DefaultDialer.Dial(r.url, header)
	if err != nil {
		log.Printf("[RealtimeClient] WebSocket dial fail, url: %s, rsp: %v, err: %v\n", r.url, rsp, err)
		return err
	}
	c.SetCloseHandler(func(code int, reason string) error {
		log.Printf("[RealtimeClient] WebSocket closed with code: %d, reason: %s\n", code, reason)
		return nil
	})
	r.conn, r.isConnected, r.wg = c, true, &sync.WaitGroup{}

	r.wg.Add(1)
	go r.readWsMsg()

	return nil
}

func (r *realtimeClient) IsConnected() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.isConnected
}

func (r *realtimeClient) Disconnect() (err error) {
	r.lock.Lock()
	defer r.lock.Unlock()
	if !r.isConnected {
		return nil
	}
	r.isConnected = false
	return r.conn.Close()
}

func (r *realtimeClient) Wait() {
	log.Printf("[RealtimeClient] Waiting for exit with timeout %v ...\n", waitTimeout)

	done := make(chan struct{})
	go func() {
		defer close(done) // Ensure channel is closed when Wait() returns
		r.wg.Wait()
	}()

	select {
	case <-done:
		log.Printf("[RealtimeClient] Exited normally.")
	case <-time.After(waitTimeout):
		log.Printf("[RealtimeClient] Wait timed out after %v.", waitTimeout)
		// Consider adding further action if timeout occurs, e.g., cancelling context or returning an error
	}
}

func (r *realtimeClient) Send(event *events.Event) (err error) {
	r.lock.RLock()
	defer r.lock.RUnlock()
	if !r.isConnected {
		log.Printf("[RealtimeClient] Sending event fail, err: not connected\n")
		return fmt.Errorf("not connected")
	}
	if event.ClientTimestamp <= 0 {
		event.ClientTimestamp = time.Now().UnixMilli()
	}
	if err = r.conn.WriteMessage(websocket.TextMessage, []byte(event.ToJson())); err != nil {
		log.Printf("[RealtimeClient] Send failed, error: %v\n", err)
	}
	return err
}

func (r *realtimeClient) SendFrameByVideo(event *events.Event) (err error) {
	if events.RealtimeClientVideoAppend != event.Type {
		return fmt.Errorf("event type is not RealtimeClientVideoAppend")
	}
	if len(event.VideoFrame) == 0 {
		return fmt.Errorf("event videoFrame is empty")
	}

	if event.ClientTimestamp <= 0 {
		event.ClientTimestamp = time.Now().UnixMilli()
	}

	r.videoFrameMutex.Lock()
	r.videoFrames = append(r.videoFrames, event.VideoFrame)
	frameCount := len(r.videoFrames)
	r.videoFrameMutex.Unlock()

	log.Printf("[SendFrameByVideo] Frame collected, total frames: %d, frame size: %d bytes\n", frameCount, len(event.VideoFrame))

	if frameCount >= r.maxFrameCount {
		log.Printf("[SendFrameByVideo] Auto-flushing %d frames\n", frameCount)
		return r.FlushVideoFrames()
	}

	return nil
}

func (r *realtimeClient) FlushVideoFrames() error {
	r.videoFrameMutex.Lock()
	frames := make([][]byte, len(r.videoFrames))
	copy(frames, r.videoFrames)
	r.videoFrames = r.videoFrames[:0]
	r.videoFrameMutex.Unlock()

	if len(frames) == 0 {
		log.Printf("[FlushVideoFrames] No frames to flush\n")
		return nil
	}

	log.Printf("[FlushVideoFrames] Flushing %d frames to API\n", len(frames))

	var contentArray []map[string]interface{}
	prompt := r.instructions
	if prompt == "" {
		prompt = "请描述这个视频的内容"
	}
	contentArray = append(contentArray, map[string]interface{}{
		"type": "text",
		"text": prompt,
	})

	for i, frame := range frames {
		frameBase64 := base64.StdEncoding.EncodeToString(frame)
		contentArray = append(contentArray, map[string]interface{}{
			"type": "image_url",
			"image_url": map[string]string{
				"url": fmt.Sprintf("data:image/jpeg;base64,%s", frameBase64),
			},
		})
		log.Printf("[FlushVideoFrames] Frame %d size: %d bytes, base64 size: %d\n", i+1, len(frame), len(frameBase64))
	}

	return r.sendBatchFramesTo4V(contentArray)
}

func (r *realtimeClient) sendBatchFramesTo4V(content []map[string]interface{}) error {

	apiURL := "https://open.bigmodel.cn/api/paas/v4/chat/completions"

	requestBody := map[string]interface{}{
		"model": "glm-4.5v",
		"messages": []map[string]interface{}{
			{
				"role":    "user",
				"content": content,
			},
		},
		"stream": true,
	}

	bodyBytes, err := json.Marshal(requestBody)
	if err != nil {
		log.Printf("[FlushVideoFrames] Failed to marshal request body: %v\n", err)
		return err
	}

	// 创建 HTTP 请求
	req, err := http.NewRequest("POST", apiURL, bytes.NewReader(bodyBytes))
	if err != nil {
		log.Printf("[FlushVideoFrames] Failed to create request: %v\n", err)
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.apiKey))

	log.Printf("[FlushVideoFrames] Sending batch request to API...\n")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[FlushVideoFrames] Failed to send request: %v\n", err)
		return err
	}
	defer resp.Body.Close()

	log.Printf("[FlushVideoFrames] Got response with status: %d\n", resp.StatusCode)

	// 如果状态码不是 200，读取错误信息
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("[FlushVideoFrames] API Error: %s\n", string(body))
		return fmt.Errorf("API Error: %s", string(body))
	}

	// 读取流式响应
	reader := bufio.NewReader(resp.Body)
	responseID := fmt.Sprintf("resp%d", time.Now().UnixNano())
	itemID := fmt.Sprintf("item%d", time.Now().UnixNano())

	r.sendFakeEvent(&events.Event{
		EventID:         fmt.Sprintf("event%d", time.Now().UnixNano()),
		Type:            "response.created",
		ClientTimestamp: time.Now().UnixMilli(),
		ResponseID:      responseID,
	})

	r.sendFakeEvent(&events.Event{
		EventID:         fmt.Sprintf("event%d", time.Now().UnixNano()),
		Type:            "response.output_item.added",
		ClientTimestamp: time.Now().UnixMilli(),
		ResponseID:      responseID,
		ItemID:          itemID,
	})

	fullText := ""
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("[FlushVideoFrames] Error reading response: %v\n", err)
			}
			break
		}

		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}

		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		// 提取增量文本
		if choices, ok := chunk["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if delta, ok := choice["delta"].(map[string]interface{}); ok {
					if content, ok := delta["content"].(string); ok && content != "" {
						fullText += content

						r.sendFakeEvent(&events.Event{
							EventID:         fmt.Sprintf("event%d", time.Now().UnixNano()),
							Type:            "response.text.delta",
							ClientTimestamp: time.Now().UnixMilli(),
							ResponseID:      responseID,
							ItemID:          itemID,
							Delta:           content,
						})
					}
				}
			}
		}
	}

	textCopy := fullText
	r.sendFakeEvent(&events.Event{
		EventID:         fmt.Sprintf("event%d", time.Now().UnixNano()),
		Type:            "response.text.done",
		ClientTimestamp: time.Now().UnixMilli(),
		ResponseID:      responseID,
		ItemID:          itemID,
		Text:            &textCopy,
	})

	r.sendFakeEvent(&events.Event{
		EventID:         fmt.Sprintf("event%d", time.Now().UnixNano()),
		Type:            "response.done",
		ClientTimestamp: time.Now().UnixMilli(),
		ResponseID:      responseID,
	})

	return nil
}

func (r *realtimeClient) SetInstructions(instructions string) {
	r.instructions = instructions
	log.Printf("[RealtimeClient] Instructions set to: %s\n", instructions)
}

func (r *realtimeClient) sendFakeEvent(event *events.Event) {
	if r.onReceived != nil {
		if err := r.onReceived(event); err != nil {
			log.Printf("[SendFrameByVideo] Failed to send fake event: %v\n", err)
		}
	}
}

func (r *realtimeClient) readWsMsg() {
	defer r.wg.Done()
	deadline := time.Now().Add(waitTimeout)
	for r.IsConnected() {
		if time.Now().After(deadline) {
			log.Printf("[RealtimeClient] ReadWsMsg loop time out after %v", waitTimeout)
			return
		}

		if conn := r.conn; conn != nil {
			if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
				log.Printf("[RealtimeClient] SetReadDeadline failed: %v", err)
			}
		}
		messageType, message, err := r.conn.ReadMessage()
		if err != nil {
			log.Printf("[RealtimeClient] Read response failed, type: %d, message: %s, err: %v\n", messageType, string(message), err)
			return
		}
		// log.Printf("[RealtimeClient] Received message type: %d, message len: %d\n", messageType, len(message))
		if r.onReceived == nil {
			log.Printf("[RealtimeClient] OnReceived is nil, skipping...\n")
			continue
		}
		event := &events.Event{}
		if err = json.Unmarshal(message, event); err != nil {
			log.Printf("[RealtimeClient] Unmarshal failed, err: %v\n", err)
			_ = r.Disconnect()
			return
		}
		// 处理session.update事件，提取instructions
		if event.Type == "session.update" && event.Session != nil && event.Session.Instructions != "" {
			r.instructions = event.Session.Instructions
			log.Printf("[RealtimeClient] Updated instructions: %s\n", r.instructions)
		}

		if err = r.onReceived(event); err != nil {
			log.Printf("[RealtimeClient] OnReceived failed, err: %v\n", err)
			_ = r.Disconnect()
			return
		}
	}
}
