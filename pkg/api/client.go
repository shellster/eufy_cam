package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/shellster/eufy_cam/pkg/crypto"
	debuglog "github.com/shellster/eufy_cam/pkg/log"
	"github.com/shellster/eufy_cam/config"
)

type Client struct {
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
		privateKey:  "",
	}, nil
}

func (c *Client) Login() error {
	return c.loginWithCaptcha("", "")
}

func (c *Client) LoginWithCaptcha(captchaID, captchaAnswer string) error {
	if c.lastLoginReq != nil {
		c.lastLoginReq["captcha_id"] = captchaID
		c.lastLoginReq["answer"] = captchaAnswer
		return c.postLogin(c.lastLoginReq)
	}
	return c.loginWithCaptcha(captchaID, captchaAnswer)
}

func (c *Client) loginWithCaptcha(captchaID, captchaAnswer string) error {
	if c.token != "" && c.tokenExpiry.After(time.Now()) {
		return nil
	}

	if c.config.Eufy.Username == "" || c.config.Eufy.Password == "" {
		return fmt.Errorf("username and password must be configured")
	}

	_, timezoneOffset := time.Now().Zone()
	timezoneOffset = -timezoneOffset / 60

	encPassword, err := c.encryptPassword(c.config.Eufy.Password)
	if err != nil {
		return fmt.Errorf("failed to encrypt password: %w", err)
	}

	loginData := map[string]interface{}{
		"ab":              c.config.Eufy.Country,
		"client_secret_info": map[string]string{
			"public_key": c.publicKey,
		},
		"enc":             0,
		"email":           c.config.Eufy.Username,
		"password":        encPassword,
		"time_zone":       -timezoneOffset * 60 * 1000,
		"transaction":     strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	if c.config.Eufy.VerifyCode != "" {
		loginData["verify_code"] = c.config.Eufy.VerifyCode
	}

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
		var loginData LoginResponse
		if err := json.Unmarshal(apiResp.Data, &loginData); err != nil {
			return fmt.Errorf("failed to parse login data: %w", err)
		}
		c.token = loginData.AuthToken
		c.tokenExpiry = time.Unix(loginData.TokenExpiresAt, 0)
		c.userID = loginData.UserID
		// Update shared secret with per-session server public key from login response
		if loginData.ServerSecret.PublicKey != "" {
			if secret, err := crypto.ComputeSecret(c.privateKey, loginData.ServerSecret.PublicKey); err == nil {
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
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	_, timezoneOffset := time.Now().Zone()
	timezoneOffset = -timezoneOffset / 60

	data := map[string]interface{}{
		"device_sn": "",
		"num":        1000,
		"orderby":   "",
		"page":      0,
		"station_sn": "",
		"time_zone":  -timezoneOffset * 60 * 1000,
		"transaction": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	resp, err := c.post("/v2/house/station_list", data)
	debuglog.Debugf("stations raw response (first 300): %s", string(resp.Body[:min(len(resp.Body), 300)]))
	if err != nil {
		return nil, err
	}

	var apiResp APIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse stations response: %w", err)
	}

	if apiResp.Code != CodeOK {
		return nil, fmt.Errorf("get stations failed: code %d, msg: %s", apiResp.Code, apiResp.Msg)
	}

	debuglog.Debugf("stations data (first 200): %s", string(apiResp.Data[:min(len(apiResp.Data), 200)]))
	debuglog.Debugf("sharedSecret len=%d first16=%x", len(c.sharedSecret), c.sharedSecret[:min(len(c.sharedSecret), 16)])
	decrypted, err := c.decryptAPIResponse(apiResp.Data)
	if err != nil {
		return nil, err
	}

	debuglog.Debugf("stations decrypted (first 200): %s", string(decrypted[:min(len(decrypted), 200)]))
	// Trim trailing garbage after valid JSON
	decrypted = trimJSON(decrypted)

	var stations []Station
	if err := json.Unmarshal(decrypted, &stations); err != nil {
		return nil, fmt.Errorf("failed to parse stations: %w", err)
	}

	return stations, nil
}

func (c *Client) GetDevices() ([]Device, error) {
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	_, timezoneOffset := time.Now().Zone()
	timezoneOffset = -timezoneOffset / 60

	data := map[string]interface{}{
		"device_sn": "",
		"num":        1000,
		"orderby":   "",
		"page":      0,
		"station_sn": "",
		"time_zone":  -timezoneOffset * 60 * 1000,
		"transaction": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	resp, err := c.post("/v2/house/device_list", data)
	if err != nil {
		return nil, err
	}

	var apiResp APIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse devices response: %w", err)
	}

	if apiResp.Code != CodeOK {
		return nil, fmt.Errorf("get devices failed: code %d", apiResp.Code)
	}

	debuglog.Debugf("stations data (first 200): %s", string(apiResp.Data[:min(len(apiResp.Data), 200)]))
	debuglog.Debugf("sharedSecret len=%d first16=%x", len(c.sharedSecret), c.sharedSecret[:min(len(c.sharedSecret), 16)])
	decrypted, err := c.decryptAPIResponse(apiResp.Data)
	if err != nil {
		return nil, err
	}

	decrypted = trimJSON(decrypted)

	var devices []Device
	if err := json.Unmarshal(decrypted, &devices); err != nil {
		return nil, fmt.Errorf("failed to parse devices: %w", err)
	}

	return devices, nil
}

func (c *Client) GetDSKKeys(stationSN string) (*DSKKey, error) {
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
		decrypted = trimJSON(decrypted)
		if err := json.Unmarshal(decrypted, &dskResp); err != nil {
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
	if !c.isAuthenticated() {
		return nil, fmt.Errorf("not authenticated")
	}

	data := map[string]interface{}{
		"cipher_ids":  cipherIDs,
		"user_id":     userID,
		"transaction": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	resp, err := c.post("/v2/app/cipher/get_ciphers", data)
	if err != nil {
		return nil, fmt.Errorf("get ciphers request failed: %w", err)
	}

	var apiResp APIResponse
	if err := json.Unmarshal(resp.Body, &apiResp); err != nil {
		return nil, fmt.Errorf("failed to parse ciphers response: %w", err)
	}

	if apiResp.Code != CodeOK {
		return nil, fmt.Errorf("get ciphers failed: code %d", apiResp.Code)
	}

	decrypted, err := c.decryptAPIResponse(apiResp.Data)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt ciphers: %w", err)
	}

	decrypted = trimJSON(decrypted)

	var ciphersList []Cipher
	if err := json.Unmarshal(decrypted, &ciphersList); err != nil {
		return nil, fmt.Errorf("failed to parse ciphers list: %w", err)
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
		Body:      body,
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
