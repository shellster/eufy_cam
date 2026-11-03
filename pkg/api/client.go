package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/shellster/eufy_cam/config"
	"github.com/shellster/eufy_cam/pkg/crypto"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
)

// Client talks to the Eufy cloud API. It is used concurrently (login from web
// handlers, cipher lookups from P2P goroutines), so all mutable state is
// guarded by mu: exported methods lock and delegate to unexported
// implementations that assume the lock is held.
type Client struct {
	mu           sync.Mutex
	config       *config.Config
	apiBase      string
	token        string
	tokenExpiry  time.Time
	client       *http.Client
	privateKey   string
	publicKey    string
	sharedSecret []byte
	userID       string
	lastLoginReq map[string]interface{}
}

func NewClient(cfg *config.Config) (*Client, error) {
	return &Client{
		config:     cfg,
		apiBase:    "https://extend.eufylife.com",
		token:      "",
		client:     &http.Client{Timeout: 30 * time.Second},
		privateKey: "",
	}, nil
}

func (c *Client) Login() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.login("", "")
}

func (c *Client) LoginWithCaptcha(captchaID, captchaAnswer string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.lastLoginReq != nil {
		c.lastLoginReq["captcha_id"] = captchaID
		c.lastLoginReq["answer"] = captchaAnswer
		return c.postLogin(c.lastLoginReq)
	}
	return c.login(captchaID, captchaAnswer)
}

func (c *Client) login(captchaID, captchaAnswer string) error {
	if c.token != "" && c.tokenExpiry.After(time.Now()) {
		return nil
	}

	if c.config.Eufy.Username == "" || c.config.Eufy.Password == "" {
		return fmt.Errorf("username and password must be configured")
	}

	encPassword, err := c.encryptPassword(c.config.Eufy.Password)
	if err != nil {
		return fmt.Errorf("failed to encrypt password: %w", err)
	}

	loginData := c.baseParams()
	loginData["ab"] = c.config.Eufy.Country
	loginData["client_secret_info"] = map[string]string{
		"public_key": c.publicKey,
	}
	loginData["enc"] = 0
	loginData["email"] = c.config.Eufy.Username
	loginData["password"] = encPassword

	if captchaID != "" && captchaAnswer != "" {
		loginData["captcha_id"] = captchaID
		loginData["answer"] = captchaAnswer
	}

	// Save for potential captcha retry
	c.lastLoginReq = loginData

	return c.postLogin(loginData)
}

func (c *Client) postLogin(loginData map[string]interface{}) error {
	resp, err := c.post("/v2/passport/login_sec", loginData)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("login failed with status %d: %s", resp.StatusCode, resp.Body)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return fmt.Errorf("failed to parse login response: %w", err)
	}

	switch apiResp.Code {
	case CodeOK:
		var data LoginResponse
		if err := json.Unmarshal(apiResp.Data, &data); err != nil {
			return fmt.Errorf("failed to parse login data: %w", err)
		}
		c.token = data.AuthToken
		c.tokenExpiry = time.Unix(data.TokenExpiresAt, 0)
		c.userID = data.UserID
		// Update shared secret with per-session server public key from login response
		if data.ServerSecret.PublicKey != "" {
			if secret, err := crypto.ComputeSecret(c.privateKey, data.ServerSecret.PublicKey); err == nil {
				c.sharedSecret = secret
				debuglog.Debugf("updated sharedSecret from server session key")
			}
		}

		return nil

	case CodeNeedVerifyCode:
		return &LoginError{Code: CodeNeedVerifyCode, Message: "2FA required: verify code needed"}

	case CodeLoginNeedCaptcha, CodeLoginCaptchaError:
		var captcha CaptchaChallenge
		var captchaData []byte
		var dataStr string
		if err := json.Unmarshal(apiResp.Data, &dataStr); err == nil {
			decrypted, decErr := crypto.DecryptAPIData(dataStr, c.sharedSecret)
			if decErr == nil {
				captchaData = decrypted
			} else {
				captchaData = apiResp.Data
			}
		} else {
			captchaData = apiResp.Data
		}
		if err := json.Unmarshal(captchaData, &captcha); err != nil {
			captcha.CaptchaID = ""
		}
		if captcha.CaptchaID == "" {
			captcha.CaptchaID = fmt.Sprintf("%d", apiResp.Code)
		}
		return &LoginError{Code: apiResp.Code, Message: apiResp.Msg, Captcha: &captcha}

	default:
		return &LoginError{Code: apiResp.Code, Message: apiResp.Msg}
	}
}

// baseParams returns the timezone/transaction boilerplate shared by all
// cloud API requests.
func (c *Client) baseParams() map[string]interface{} {
	_, timezoneOffset := time.Now().Zone()
	timezoneOffset = -timezoneOffset / 60
	return map[string]interface{}{
		"transaction": strconv.FormatInt(time.Now().UnixMilli(), 10),
		"time_zone":   -timezoneOffset * 60 * 1000,
	}
}

// postDecoded runs the shared response pipeline: POST → status → APIResponse
// → code check → decrypt → trimJSON → unmarshal into out.
func (c *Client) postDecoded(endpoint string, params map[string]interface{}, out interface{}) error {
	resp, err := c.post(endpoint, params)
	if err != nil {
		return err
	}

	var apiResp APIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return fmt.Errorf("failed to parse %s response: %w", endpoint, err)
	}

	if apiResp.Code != CodeOK {
		return fmt.Errorf("%s failed: code %d, msg: %s", endpoint, apiResp.Code, apiResp.Msg)
	}

	decrypted, err := c.decryptAPIResponse(apiResp.Data)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(trimJSON(decrypted), out); err != nil {
		return fmt.Errorf("failed to parse %s data: %w", endpoint, err)
	}

	return nil
}

func (c *Client) FetchCaptchaImage(captchaID string) ([]byte, error) {
	urls := []string{
		fmt.Sprintf("https://security-app.eufylife.com/v1/captcha/app/get?captcha_id=%s", captchaID),
		fmt.Sprintf("%s/v1/captcha/app/get?captcha_id=%s", c.apiBase, captchaID),
		fmt.Sprintf("%s/v1/captcha/get?captcha_id=%s", c.apiBase, captchaID),
	}
	for _, u := range urls {
		resp, err := c.client.Get(u)
		if err != nil {
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK && len(body) > 100 {
			return body, nil
		}
	}
	return nil, fmt.Errorf("could not fetch captcha image")
}

func trimJSON(data []byte) []byte {
	start := -1
	for i, b := range data {
		if b == '[' || b == '{' {
			start = i
			break
		}
	}
	if start == -1 {
		return data
	}

	open := data[start]
	close := byte(']')
	if open == '{' {
		close = byte('}')
	}

	depth := 0
	inString := false
	escape := false
	for i := start; i < len(data); i++ {
		b := data[i]
		if escape {
			escape = false
			continue
		}
		if b == '\\' && inString {
			escape = true
			continue
		}
		if b == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch b {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return data[start : i+1]
			}
		}
	}
	return data[start:]
}

func (c *Client) GetStations() ([]Station, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getStations()
}

func (c *Client) getStations() ([]Station, error) {
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	params := c.baseParams()
	params["device_sn"] = ""
	params["num"] = 1000
	params["orderby"] = ""
	params["page"] = 0
	params["station_sn"] = ""

	var stations []Station
	if err := c.postDecoded("/v2/house/station_list", params, &stations); err != nil {
		return nil, err
	}
	return stations, nil
}

func (c *Client) GetDevices() ([]Device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getDevices()
}

func (c *Client) getDevices() ([]Device, error) {
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	params := c.baseParams()
	params["device_sn"] = ""
	params["num"] = 1000
	params["orderby"] = ""
	params["page"] = 0
	params["station_sn"] = ""

	var devices []Device
	if err := c.postDecoded("/v2/house/device_list", params, &devices); err != nil {
		return nil, err
	}
	return devices, nil
}

func (c *Client) GetDSKKeys(stationSN string) (*DSKKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getDSKKeys(stationSN)
}

func (c *Client) getDSKKeys(stationSN string) (*DSKKey, error) {
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	data := map[string]interface{}{
		"invalid_dsks": map[string]string{
			stationSN: "",
		},
		"station_sns": []string{stationSN},
		"transaction": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	resp, err := c.post("/v1/app/equipment/get_dsk_keys", data)
	if err != nil {
		return nil, err
	}

	var apiResp APIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse DSK response: %w", err)
	}

	if apiResp.Code != CodeOK {
		return nil, fmt.Errorf("get DSK keys failed: code %d", apiResp.Code)
	}

	type dskResponse struct {
		DSKKeys []DSKKey `json:"dsk_keys"`
	}

	var dskResp dskResponse
	if err := json.Unmarshal(apiResp.Data, &dskResp); err != nil {
		decrypted, decErr := c.decryptAPIResponse(apiResp.Data)
		if decErr != nil {
			return nil, decErr
		}
		if err := json.Unmarshal(trimJSON(decrypted), &dskResp); err != nil {
			return nil, fmt.Errorf("failed to parse DSK keys: %w", err)
		}
	}

	for _, dskKey := range dskResp.DSKKeys {
		if dskKey.StationSN == stationSN {
			return &dskKey, nil
		}
	}

	return nil, fmt.Errorf("DSK key not found for station %s", stationSN)
}

func (c *Client) GetCiphers(cipherIDs []int, userID string) (map[int]*Cipher, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getCiphers(cipherIDs, userID)
}

func (c *Client) getCiphers(cipherIDs []int, userID string) (map[int]*Cipher, error) {
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	data := map[string]interface{}{
		"cipher_ids":  cipherIDs,
		"user_id":     userID,
		"transaction": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	var ciphersList []Cipher
	if err := c.postDecoded("/v2/app/cipher/get_ciphers", data, &ciphersList); err != nil {
		return nil, err
	}

	result := make(map[int]*Cipher)
	for i := range ciphersList {
		result[ciphersList[i].CipherID] = &ciphersList[i]
	}
	return result, nil
}

func (c *Client) GetCipher(cipherID int, userID string) (*Cipher, error) {
	ciphers, err := c.GetCiphers([]int{cipherID}, userID)
	if err != nil {
		return nil, err
	}
	return ciphers[cipherID], nil
}

func (c *Client) GetAPIBase(country string) (string, error) {
	type domainData struct {
		Domain string `json:"domain"`
	}
	type domainResponse struct {
		Code int        `json:"code"`
		Data domainData `json:"data"`
	}

	resp, err := c.client.Get(fmt.Sprintf("https://extend.eufylife.com/domain/%s", country))
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var domainResp domainResponse
	if err := json.Unmarshal(body, &domainResp); err != nil {
		return "", err
	}

	if domainResp.Code != CodeOK {
		return "", fmt.Errorf("failed to get API domain: code %d", domainResp.Code)
	}

	return fmt.Sprintf("https://%s", domainResp.Data.Domain), nil
}

func (c *Client) isAuthenticated() bool {
	return c.token != "" && c.tokenExpiry.After(time.Now())
}

func (c *Client) encryptPassword(password string) (string, error) {
	sharedSecret, err := crypto.ComputeSecret(c.privateKey, crypto.ServerPublicKey)
	if err != nil {
		return "", fmt.Errorf("failed to compute shared secret: %w", err)
	}

	encrypted, err := crypto.EncryptCBC([]byte(password), sharedSecret)
	if err != nil {
		return "", fmt.Errorf("failed to encrypt password: %w", err)
	}

	return encrypted, nil
}

func (c *Client) decryptAPIResponse(data []byte) ([]byte, error) {
	var dataStr string
	if err := json.Unmarshal(data, &dataStr); err != nil {
		return nil, fmt.Errorf("failed to unmarshal data field: %w", err)
	}
	return crypto.DecryptAPIData(dataStr, c.sharedSecret)
}

func (c *Client) post(endpoint string, data map[string]interface{}) (*HTTPResponse, error) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	u, err := url.Parse(c.apiBase + endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse URL: %w", err)
	}

	req, err := http.NewRequest("POST", u.String(), bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("App_version", "v4.6.0_1630")
	req.Header.Set("Os_type", "android")
	req.Header.Set("Os_version", "31")
	req.Header.Set("Phone_model", "ONEPLUS A3003")
	req.Header.Set("Country", c.config.Eufy.Country)
	req.Header.Set("Language", c.config.Eufy.Language)
	req.Header.Set("Openudid", "5e4621b0152c0d00")
	req.Header.Set("Net_type", "wifi")
	req.Header.Set("Mnc", "02")
	req.Header.Set("Mcc", "262")
	req.Header.Set("Sn", "75814221ee75")
	req.Header.Set("Model_type", "PHONE")
	req.Header.Set("Cache-Control", "no-cache")

	if c.token != "" {
		req.Header.Set("X-Auth-Token", c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	return &HTTPResponse{
		StatusCode: resp.StatusCode,
		Body:       body,
	}, nil
}

type HTTPResponse struct {
	StatusCode int
	Body       []byte
}

func (c *Client) initialize() error {
	keyPair, err := crypto.GenerateKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate ECDH keys: %w", err)
	}

	c.privateKey = keyPair.PrivateKey
	c.publicKey = keyPair.PublicKey

	secret, err := crypto.ComputeSecret(c.privateKey, crypto.ServerPublicKey)
	if err != nil {
		return fmt.Errorf("failed to compute shared secret: %w", err)
	}
	c.sharedSecret = secret

	regionAPI, err := c.GetAPIBase(c.config.Eufy.Country)
	if err != nil {
		return fmt.Errorf("failed to get API base: %w", err)
	}

	c.apiBase = regionAPI

	return nil
}

func Init(cfg *config.Config) (*Client, error) {
	client, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}

	if err := client.initialize(); err != nil {
		return nil, err
	}

	return client, nil
}
