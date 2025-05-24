package matrixdll

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/google/uuid"
)

const (
	loginPath     = "/_matrix/client/r0/login"
	sendEventPath = "/_matrix/client/r0/rooms/%s/send/%s/%s"
)

type loginReq struct {
	Type       string                 `json:"type"`
	Identifier map[string]interface{} `json:"identifier"`
	Password   string                 `json:"password"`
}
type loginResp struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

// MatrixClient - структура для управления signaling
type MatrixClient struct {
	mu          sync.Mutex
	Homeserver  string
	AccessToken string
	UserID      string
	RoomID      string

	CurrentCallID string
	MyPartyID     string
}

// NewMatrixClient - логин в Matrix и создание клиента
func NewMatrixClient(homeserver, username, password, roomID string) (*MatrixClient, error) {
	url := homeserver + loginPath
	req := loginReq{
		Type:       "m.login.password",
		Identifier: map[string]interface{}{"type": "m.id.user", "user": username},
		Password:   password,
	}
	var resp loginResp
	if err := postJSON(url, req, &resp); err != nil {
		return nil, fmt.Errorf("login error: %w", err)
	}

	client := &MatrixClient{
		Homeserver:  homeserver,
		AccessToken: resp.AccessToken,
		UserID:      resp.UserID,
		RoomID:      roomID,
		MyPartyID:   uuid.NewString(),
	}
	return client, nil
}

// SendCallInvite - отправить приглашение на звонок (offer)
// sdpOffer - строка SDP, которую сформирует Android
func (c *MatrixClient) SendCallInvite(sdpOffer string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.CurrentCallID = uuid.NewString()
	invite := map[string]interface{}{
		"call_id":  c.CurrentCallID,
		"party_id": c.MyPartyID,
		"lifetime": 120000,
		"offer":    map[string]interface{}{"type": "offer", "sdp": sdpOffer},
		"version":  "1",
	}
	txn := uuid.NewString()
	url := fmt.Sprintf(c.Homeserver+sendEventPath, c.RoomID, "m.call.invite", txn)
	if err := postJSON(url, invite, &map[string]interface{}{}); err != nil {
		return "", err
	}
	return c.CurrentCallID, nil
}

// SendCallAnswer - отправить ответ (answer)
// sdpAnswer - строка SDP, которую сформирует Android
func (c *MatrixClient) SendCallAnswer(sdpAnswer string, callID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if callID != "" {
		c.CurrentCallID = callID
	}
	answer := map[string]interface{}{
		"call_id":  c.CurrentCallID,
		"party_id": c.MyPartyID,
		"answer":   map[string]interface{}{"type": "answer", "sdp": sdpAnswer},
		"version":  "1",
	}
	txn := uuid.NewString()
	url := fmt.Sprintf(c.Homeserver+sendEventPath, c.RoomID, "m.call.answer", txn)
	return postJSON(url, answer, &map[string]interface{}{})
}

// SendCandidates - отправить ICE-кандидаты (candidates - массив словарей с candidate, sdpMid, sdpMLineIndex)
func (c *MatrixClient) SendCandidates(candidates []map[string]interface{}, callID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if callID != "" {
		c.CurrentCallID = callID
	}
	payload := map[string]interface{}{
		"call_id":    c.CurrentCallID,
		"party_id":   c.MyPartyID,
		"version":    "1",
		"candidates": candidates,
	}
	txn := uuid.NewString()
	url := fmt.Sprintf(c.Homeserver+sendEventPath, c.RoomID, "m.call.candidates", txn)
	return postJSON(url, payload, &map[string]interface{}{})
}

// Вспомогательная функция POST
func postJSON(url string, payload interface{}, out interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// GetUserID - получить user_id после логина
func (c *MatrixClient) GetUserID() string {
	return c.UserID
}

// GetAccessToken - получить access_token после логина
func (c *MatrixClient) GetAccessToken() string {
	return c.AccessToken
}

// GetPartyID - получить party_id (уникальный для этого клиента)
func (c *MatrixClient) GetPartyID() string {
	return c.MyPartyID
}

// GetCallID - получить последний call_id (или текущий)
func (c *MatrixClient) GetCallID() string {
	return c.CurrentCallID
}
