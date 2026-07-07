package model

import "time"

type ActorRole string

const (
	RoleAdmin       ActorRole = "admin"
	RoleReseller    ActorRole = "reseller"
	RoleSubReseller ActorRole = "subreseller"
)

type Actor struct {
	TelegramID int64     `json:"telegram_id"`
	Name       string    `json:"name"`
	Role       ActorRole `json:"role"`
	ParentID   int64     `json:"parent_id"`
	IsAdmin    bool      `json:"is_admin"`
}

type Account struct {
	ID               int64      `json:"id"`
	Username         string     `json:"username"`
	Password         string     `json:"password"`
	UUID             string     `json:"uuid"`
	LimitConnections int        `json:"limit_connections"`
	ExpiresAt        time.Time  `json:"expires_at"`
	ExpiryDate       string     `json:"expiry_date"`
	OwnerTelegramID  int64      `json:"owner_telegram_id"`
	OwnerName        string     `json:"owner_name"`
	OwnerType        string     `json:"owner_type"`
	Status           string     `json:"status"`
	IsTrial          bool       `json:"is_trial"`
	XrayEnabled      bool       `json:"xray_enabled"`
	CreditCounted    bool       `json:"credit_counted"`
	ClientWhatsApp   string     `json:"client_whatsapp"`
	MonthlyValue     float64    `json:"monthly_value"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

type Reseller struct {
	ID                       int64      `json:"id"`
	TelegramID               int64      `json:"telegram_id"`
	Name                     string     `json:"name"`
	WhatsAppPhone            string     `json:"whatsapp_phone"`
	Password                 string     `json:"password"`
	Credits                  int        `json:"credits"`
	Active                   bool       `json:"active"`
	MaxDays                  int        `json:"max_days"`
	MaxLimit                 int        `json:"max_limit"`
	AllowXray                bool       `json:"allow_xray"`
	AllowSubReseller         bool       `json:"allow_subreseller"`
	ExpiresAt                time.Time  `json:"expires_at"`
	ParentTelegramID         int64      `json:"parent_telegram_id"`
	Level                    int        `json:"level"`
	MonthlyPrice             float64    `json:"monthly_price"`
	PendingMonthlyPrice      float64    `json:"pending_monthly_price"`
	PendingMonthlyDifference float64    `json:"pending_monthly_difference"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	DeletedAt                *time.Time `json:"deleted_at,omitempty"`
}

type Server struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Host        string    `json:"host"`
	SSHPort     int       `json:"ssh_port"`
	SSHUser     string    `json:"ssh_user"`
	SSHPassword string    `json:"ssh_password"`
	AgentPort   int       `json:"agent_port"`
	AgentToken  string    `json:"agent_token"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Device struct {
	ID               string    `json:"id"`
	Username         string    `json:"username"`
	LimitConnections int       `json:"limit_connections"`
	UserUUID         string    `json:"user_uuid"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Scope struct {
	ActorID            int64
	Role               ActorRole
	IncludeDescendants bool
}

type App struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Version      string    `json:"version"`
	FileID       string    `json:"file_id"`
	FileUniqueID string    `json:"file_unique_id"`
	FileName     string    `json:"file_name"`
	MimeType     string    `json:"mime_type"`
	Path         string    `json:"path"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type NoticeEvent struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	Targets   int       `json:"targets"`
	Delivered int       `json:"delivered"`
	Failed    int       `json:"failed"`
	CreatedAt time.Time `json:"created_at"`
}

type PaymentOwnerConfig struct {
	OwnerID   int64     `json:"owner_id"`
	Bank      string    `json:"bank"`
	Enabled   bool      `json:"enabled"`
	Token     string    `json:"token,omitempty"`
	DataJSON  string    `json:"data_json"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PaymentPackage struct {
	ID        int64     `json:"id"`
	OwnerID   int64     `json:"owner_id"`
	Kind      string    `json:"kind"`
	Name      string    `json:"name"`
	Months    int       `json:"months"`
	Days      int       `json:"days"`
	Credits   int       `json:"credits"`
	Amount    float64   `json:"amount"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type PaymentWebhookEvent struct {
	ID          int64     `json:"id"`
	EventID     string    `json:"event_id"`
	OrderID     string    `json:"order_id"`
	OwnerID     int64     `json:"owner_id"`
	Bank        string    `json:"bank"`
	ExternalID  string    `json:"external_id"`
	Status      string    `json:"status"`
	RemoteIP    string    `json:"remote_ip"`
	Result      string    `json:"result"`
	ErrorText   string    `json:"error_text"`
	HeadersJSON string    `json:"headers_json"`
	BodyJSON    string    `json:"body_json"`
	CreatedAt   time.Time `json:"created_at"`
}

type PaymentOrder struct {
	OrderID          string     `json:"order_id"`
	OwnerID          int64      `json:"owner_id"`
	TargetResellerID int64      `json:"target_reseller_id"`
	Kind             string     `json:"kind"`
	Months           int        `json:"months"`
	Days             int        `json:"days"`
	Credits          int        `json:"credits"`
	Amount           float64    `json:"amount"`
	Bank             string     `json:"bank"`
	ExternalID       string     `json:"external_id"`
	Status           string     `json:"status"`
	PixCopyPaste     string     `json:"pix_copy_paste"`
	PaymentURL       string     `json:"payment_url"`
	PayloadJSON      string     `json:"payload_json"`
	CreatedAt        time.Time  `json:"created_at"`
	PaidAt           *time.Time `json:"paid_at,omitempty"`
	AppliedAt        *time.Time `json:"applied_at,omitempty"`
}
