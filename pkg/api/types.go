package api

import (
	"encoding/json"
	"fmt"
)

type Station struct {
	StationSN   string            `json:"station_sn"`
	P2PDID      string            `json:"p2p_did"`
	AppConn      string            `json:"app_conn"`
	Name        string            `json:"station_name"`
	Model        string            `json:"model"`
	HWVersion    string            `json:"hw_version"`
	SWVersion    string            `json:"sw_version"`
	Member       StationMember      `json:"member"`
}

type StationMember struct {
	AdminUserID string `json:"admin_user_id"`
}

type Device struct {
	DeviceSN      string `json:"device_sn"`
	DeviceType    int    `json:"device_type"`
	DeviceName    string `json:"device_name"`
	Channel       int    `json:"device_channel"`
	StationSN     string `json:"station_sn"`
	Model         string `json:"model"`
	HWVersion     string `json:"hw_version"`
	SWVersion     string `json:"sw_version"`
}

type LoginResponse struct {
	AuthToken       string `json:"auth_token"`
	TokenExpiresAt int64  `json:"token_expires_at"`
	UserID         string `json:"user_id"`
	Email          string `json:"email"`
	NickName       string `json:"nick_name"`
	ServerSecret   ServerSecret `json:"server_secret_info"`
}

type ServerSecret struct {
	PublicKey string `json:"public_key"`
}

type APIResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

type DSKKey struct {
	StationSN  string `json:"station_sn"`
	DSKKey     string `json:"dsk_key"`
	Expiration int64  `json:"expiration"`
}

type CaptchaChallenge struct {
	CaptchaID     string `json:"captcha_id"`
	Item          string `json:"item"`
	CaptchaImg    string `json:"captcha_img"`
	CaptchaURL    string `json:"captcha_url"`
	CaptchaBase64 string `json:"captcha_base64"`
}

type LoginError struct {
	Code    int
	Message string
	Captcha *CaptchaChallenge
}

func (e *LoginError) Error() string {
	return fmt.Sprintf("login error: code %d, msg %s", e.Code, e.Message)
}

type Cipher struct {
	CipherID   int    `json:"cipher_id"`
	UserID     string `json:"user_id"`
	PrivateKey string `json:"private_key"`
}

const (
	CodeOK                = 0
	CodeNeedVerifyCode    = 26052
	CodeLoginNeedCaptcha  = 100032
	CodeLoginCaptchaError = 100033
)
