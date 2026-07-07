package checkuser

import (
	"encoding/json"
	"html/template"
	"net/http"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/online"
	"primecel-gestor/gestor_bot/store"
)

type Server struct {
	cfg config.Config
	st  *store.DB
}

func NewServer(cfg config.Config, st *store.DB) *Server { return &Server{cfg: cfg, st: st} }
func (s *Server) Router() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.root)
	mux.HandleFunc("/check", s.check)
	mux.HandleFunc("/check/", s.checkPath)
	mux.HandleFunc("/details/", s.details)
	mux.HandleFunc("/count", s.count)
	mux.HandleFunc("/online-summary", s.onlineSummary)
	mux.HandleFunc("/onlines", s.onlineSummary)
	mux.HandleFunc("/devices/list", s.devicesList)
	mux.HandleFunc("/devices/list/", s.devicesList)
	mux.HandleFunc("/devices/count", s.devicesCount)
	mux.HandleFunc("/device", s.check)
	return mux
}
func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if user := s.findUserParam(r); user != "" {
		s.renderCheck(w, r, user)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = page.Execute(w, map[string]any{"Version": "primecel-gestor"})
}
func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	s.renderCheck(w, r, s.findUserParam(r))
}
func (s *Server) checkPath(w http.ResponseWriter, r *http.Request) {
	s.renderCheck(w, r, strings.TrimPrefix(r.URL.Path, "/check/"))
}
func (s *Server) details(w http.ResponseWriter, r *http.Request) {
	acc := s.resolveAccount(r, strings.TrimPrefix(r.URL.Path, "/details/"))
	if acc == nil {
		jsonOut(w, http.StatusNotFound, map[string]any{"status": "not_found", "allowed": false})
		return
	}
	remaining, remainingDays, remainingValue, remainingUnit, remainingHours, remainingMinutes, totalMinutes := s.remainingInfo(*acc)
	jsonOut(w, 200, map[string]any{
		"id": acc.ID, 
		"username": acc.Username, 
		"expires_at": acc.ExpiryDate, 
		"expires_days": remainingDays, 
		"expires_in": remaining, 
		"expiration_remaining": remaining, 
		"expiration_display": remaining, 
		"expiration_value": remainingValue, 
		"expiration_unit": remainingUnit, 
		"expiration_hours": remainingHours, 
		"expiration_minutes": remainingMinutes, 
		"expiration_total_minutes": totalMinutes, 
		"limit": acc.LimitConnections, 
		"connections": 0,
	})
}
func (s *Server) count(w http.ResponseWriter, r *http.Request) {
	accs, _ := s.st.ListAccounts(r.Context(), false)
	n := 0
	now := time.Now()
	for _, a := range accs {
		if a.Status == "active" && a.ExpiresAt.After(now) {
			n++
		}
	}
	jsonOut(w, 200, map[string]any{"count": n})
}
func (s *Server) onlineSummary(w http.ResponseWriter, r *http.Request) {
	mgr := online.NewManager(s.cfg, s.st)
	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	localOnly := scope == "local" || scope == "server" || scope == "self" || r.Header.Get("X-Primecel-Online-Scope") == "local"
	var (
		sum online.Summary
		err error
	)
	if localOnly {
		sum, err = mgr.LocalSummary(r.Context())
	} else {
		sum, err = mgr.Summary(r.Context())
	}
	if err != nil {
		jsonOut(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error(), "users": []any{}})
		return
	}
	jsonOut(w, 200, sum)
}
func (s *Server) devicesList(w http.ResponseWriter, r *http.Request) {
	user := s.findUserParam(r)
	if user == "" {
		// Compatibilidade com /devices/list/USUARIO.
		user = strings.TrimPrefix(r.URL.Path, "/devices/list/")
	}
	devices, err := s.st.ListDevices(r.Context(), user)
	if err != nil {
		jsonOut(w, http.StatusInternalServerError, map[string]any{"error": err.Error(), "devices": []any{}})
		return
	}
	jsonOut(w, 200, map[string]any{"count": len(devices), "devices": devices})
}
func (s *Server) devicesCount(w http.ResponseWriter, r *http.Request) {
	user := s.findUserParam(r)
	n := 0
	if user != "" {
		n, _ = s.st.CountDevices(r.Context(), user)
	}
	jsonOut(w, 200, map[string]any{"count": n})
}
func (s *Server) renderCheck(w http.ResponseWriter, r *http.Request, user string) {
	acc := s.resolveAccount(r, user)
	if acc == nil {
		jsonOut(w, 404, map[string]any{"status": "not_found", "allowed": false, "device_allowed": false, "device_status": "not_found"})
		return
	}
	device := s.findDeviceParam(r)
	deviceAllowed := true
	deviceStatus := "ok"
	deviceCount, _ := s.st.CountDevices(r.Context(), acc.Username)
	if device != "" {
		exists, _ := s.st.DeviceExists(r.Context(), acc.Username, device)
		if !exists {
			if deviceCount >= acc.LimitConnections {
				deviceAllowed = false
				deviceStatus = "limit_reached"
				deviceCount++
			} else {
				_ = s.st.AddDevice(r.Context(), acc.Username, device, acc.UUID, acc.LimitConnections)
				deviceCount++
			}
		}
	}
	allowed := acc.Status == "active" && acc.ExpiresAt.After(time.Now()) && deviceAllowed
	if !acc.ExpiresAt.After(time.Now()) {
		deviceStatus = "expired"
	}
	remaining, remainingDays, remainingValue, remainingUnit, _, _, _ := s.remainingInfo(*acc)
	liveConnections := 0
	if sum, err := online.NewManager(s.cfg, s.st).Summary(r.Context()); err == nil {
		for _, it := range sum.Users {
			if strings.EqualFold(it.Username, acc.Username) && it.Connections > liveConnections {
				liveConnections = it.Connections
			}
		}
	}
	jsonOut(w, 200, map[string]any{
		"id": acc.ID,
		"username": acc.Username, 
		"expiration_date": acc.ExpiryDate, 
		"expiration_days": remainingDays, 
		"expires_in": remaining, 
		"expiration_value": remainingValue, 
		"expiration_unit": remainingUnit, 
		"limit_connections": acc.LimitConnections, 
		"count_connections": liveConnections, 
		"device_count": deviceCount, 
		"device_limit": acc.LimitConnections, 
		"device_allowed": deviceAllowed && allowed, 
		"device_status": deviceStatus,
	})
}
func (s *Server) resolveAccount(r *http.Request, user string) *model.Account {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil
	}
	if a, _ := s.st.FindAccount(r.Context(), user); a != nil {
		return a
	}
	if looksUUID(user) {
		if a, _ := s.st.FindAccountByUUID(r.Context(), user); a != nil {
			return a
		}
	}
	return nil
}
func (s *Server) findUserParam(r *http.Request) string {
	q := r.URL.Query()
	for _, k := range []string{"user", "username", "usuario", "uuid", "xray_uuid", "id_uuid"} {
		if v := q.Get(k); v != "" {
			return v
		}
	}
	return ""
}
func (s *Server) findDeviceParam(r *http.Request) string {
	q := r.URL.Query()
	for _, k := range []string{"deviceId", "deviceid", "device_id", "hwid", "android_id", "id"} {
		if v := q.Get(k); v != "" {
			return v
		}
	}
	return ""
}
func (s *Server) remaining(a model.Account) string {
	label, _, _, _, _, _, _ := s.remainingInfo(a)
	return label
}
func (s *Server) remainingInfo(a model.Account) (string, int, int, string, int, int, int) {
	d := time.Until(a.ExpiresAt)
	if d <= 0 {
		return "expirado", 0, 0, "expired", 0, 0, 0
	}
	totalMinutes := int(d.Minutes())
	if totalMinutes < 1440 {
		h := totalMinutes / 60
		m := totalMinutes % 60
		return two(h) + "h:" + two(m), 0, 0, "days", h, m, totalMinutes
	}
	days := totalMinutes / 1440
	if days == 1 {
		return "1 dia", 1, 1, "days", 24, 0, totalMinutes
	}
	return itoa(days) + " dias", days, days, "days", days * 24, 0, totalMinutes
}
func daysRemaining(t time.Time) int {
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return int(d.Hours() / 24)
}
func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func looksUUID(s string) bool { return strings.Count(s, "-") == 4 && len(s) >= 32 }
func two(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	b := []byte{}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

var page = template.Must(template.New("p").Parse(`
<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>CheckUser</title>
<style>
body{
    background:#111;
    color:#fff;
    font-family:Arial,sans-serif;
    text-align:center;
    padding-top:50px;
}
.box{
    max-width:400px;
    margin:auto;
    border:1px solid #333;
    border-radius:12px;
    padding:20px;
    background:#1a1a1a;
}
h2{
    margin-top:0;
}
.item{
    margin:15px 0;
    padding:12px;
    background:#222;
    border-radius:8px;
}
</style>
</head>
<body>
<div class="box">
<h2>CHECKUSER</h2>

<div class="item">
TOTAL DE CONEXÕES
</div>

<div class="item">
VER DEVICE ID
</div>

</div>
</body>
</html>
`))
