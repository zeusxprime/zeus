package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"primecel-gestor/gestor_bot/config"
	"primecel-gestor/gestor_bot/model"
	"primecel-gestor/gestor_bot/store"
)

type Manager struct {
	Config config.Config
	Store  *store.DB
	Client *http.Client
}

func NewManager(cfg config.Config, st *store.DB) *Manager {
	return &Manager{Config: cfg, Store: st, Client: &http.Client{Timeout: 12 * time.Second}}
}

type SyncReport struct {
	Domain     string   `json:"domain"`
	ZoneID     string   `json:"zone_id"`
	DesiredIPs []string `json:"desired_ips"`
	Created    int      `json:"created"`
	Deleted    int      `json:"deleted"`
	Kept       int      `json:"kept"`
	DryRun     bool     `json:"dry_run"`
}

type cfResp struct {
	Success bool            `json:"success"`
	Errors  []any           `json:"errors"`
	Result  json.RawMessage `json:"result"`
}
type zone struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

const DefaultServerDomain = "vpn.primecel.shop"

func NormalizeDomain(domain string) string {
	return strings.Trim(strings.ToLower(domain), ". ")
}

func IsDefaultServerDomain(domain string) bool {
	return NormalizeDomain(domain) == DefaultServerDomain
}

var LegacyServerDomains = map[string]bool{
	"sv.primecel.shop":     true,
	"server.primecel.shop": true,
}

func IsLegacyServerDomain(domain string) bool {
	return LegacyServerDomains[NormalizeDomain(domain)]
}

func IsReservedServerDomain(domain string) bool {
	d := NormalizeDomain(domain)
	return d == DefaultServerDomain || LegacyServerDomains[d]
}

var AllowedDNSVPSDomainList = []string{
	"dns.443.primecel.shop",
	"dns.8443.primecel.shop",
	"xray.primecel.shop",
}

var AllowedDNSVPSDomains = map[string]bool{
	"dns.443.primecel.shop":  true,
	"dns.8443.primecel.shop": true,
	"xray.primecel.shop":     true,
}

func IsAllowedDNSVPSDomain(domain string) bool {
	return AllowedDNSVPSDomains[NormalizeDomain(domain)]
}

func AllowedDNSVPSDomainsInOrder() []string {
	out := make([]string, len(AllowedDNSVPSDomainList))
	copy(out, AllowedDNSVPSDomainList)
	return out
}

// EnsureServerDNSIP garante um IP no domínio padrão dos servidores.
// Ele apenas adiciona/mantém o IP informado e não remove outros registros.
func (m *Manager) EnsureServerDNSIP(ctx context.Context, ip string, dryRun bool) (SyncReport, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return SyncReport{}, errors.New("IP obrigatório")
	}
	rep := SyncReport{Domain: DefaultServerDomain, DesiredIPs: []string{ip}, DryRun: dryRun}
	if dryRun {
		return rep, nil
	}
	if strings.TrimSpace(m.cloudflareToken(ctx)) == "" {
		return rep, errors.New("token Cloudflare vazio")
	}
	z, err := m.findZone(ctx, DefaultServerDomain)
	if err != nil {
		return rep, err
	}
	rep.ZoneID = z.ID
	recs, err := m.listARecords(ctx, z.ID, DefaultServerDomain)
	if err != nil {
		return rep, err
	}
	for _, r := range recs {
		if r.Type == "A" && strings.EqualFold(r.Name, DefaultServerDomain) && strings.TrimSpace(r.Content) == ip {
			rep.Kept++
			b, _ := json.Marshal(rep)
			_ = m.Store.AddCloudflareEvent(ctx, "server_dns_add_ip", DefaultServerDomain, true, string(b))
			return rep, nil
		}
	}
	if err := m.createARecord(ctx, z.ID, DefaultServerDomain, ip, false); err != nil {
		return rep, err
	}
	rep.Created++
	b, _ := json.Marshal(rep)
	_ = m.Store.AddCloudflareEvent(ctx, "server_dns_add_ip", DefaultServerDomain, true, string(b))
	return rep, nil
}

// SyncServerDNSIPs mantém vpn.primecel.shop exatamente com os IPs ativos informados:
// adiciona IP novo, mantém IP existente e remove automaticamente IP antigo.
func (m *Manager) SyncServerDNSIPs(ctx context.Context, ips []string, dryRun bool) (SyncReport, error) {
	desired := uniqIPs(ips)
	rep := SyncReport{Domain: DefaultServerDomain, DesiredIPs: desired, DryRun: dryRun}
	if strings.TrimSpace(m.cloudflareToken(ctx)) == "" {
		return rep, errors.New("token Cloudflare vazio")
	}
	z, err := m.findZone(ctx, DefaultServerDomain)
	if err != nil {
		return rep, err
	}
	rep.ZoneID = z.ID
	recs, err := m.listARecords(ctx, z.ID, DefaultServerDomain)
	if err != nil {
		return rep, err
	}
	desiredSet := map[string]bool{}
	for _, ip := range desired {
		desiredSet[ip] = true
	}
	found := map[string]bool{}
	for _, r := range recs {
		if r.Type != "A" || !strings.EqualFold(r.Name, DefaultServerDomain) {
			continue
		}
		content := strings.TrimSpace(r.Content)
		if desiredSet[content] {
			found[content] = true
			rep.Kept++
			continue
		}
		rep.Deleted++
		if !dryRun {
			if err := m.deleteServerDNSRecordForName(ctx, z.ID, r.ID, DefaultServerDomain); err != nil {
				return rep, err
			}
		}
	}
	for _, ip := range desired {
		if found[ip] {
			continue
		}
		rep.Created++
		if !dryRun {
			if err := m.createARecord(ctx, z.ID, DefaultServerDomain, ip, false); err != nil {
				return rep, err
			}
		}
	}
	b, _ := json.Marshal(rep)
	_ = m.Store.AddCloudflareEvent(ctx, "server_dns_sync", DefaultServerDomain, true, string(b))
	return rep, nil
}

// RemoveServerDNSIP remove somente o registro A exato do IP informado em vpn.primecel.shop.
// Se outro servidor ativo ainda usa o mesmo IP, a camada Telegram não chama esta rotina.
func (m *Manager) RemoveServerDNSIP(ctx context.Context, ip string, dryRun bool) (SyncReport, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return SyncReport{}, errors.New("IP obrigatório")
	}
	rep := SyncReport{Domain: DefaultServerDomain, DesiredIPs: []string{ip}, DryRun: dryRun}
	if strings.TrimSpace(m.cloudflareToken(ctx)) == "" {
		return rep, errors.New("token Cloudflare vazio")
	}
	z, err := m.findZone(ctx, DefaultServerDomain)
	if err != nil {
		return rep, err
	}
	rep.ZoneID = z.ID
	recs, err := m.listARecords(ctx, z.ID, DefaultServerDomain)
	if err != nil {
		return rep, err
	}
	for _, r := range recs {
		if r.Type != "A" || !strings.EqualFold(r.Name, DefaultServerDomain) || strings.TrimSpace(r.Content) != ip {
			continue
		}
		rep.Deleted++
		if !dryRun {
			if err := m.deleteServerDNSRecordForName(ctx, z.ID, r.ID, DefaultServerDomain); err != nil {
				return rep, err
			}
		}
	}
	b, _ := json.Marshal(rep)
	_ = m.Store.AddCloudflareEvent(ctx, "server_dns_remove_ip", DefaultServerDomain, true, string(b))
	return rep, nil
}

// EnsureDNSVPSIP adiciona de forma estritamente aditiva um IP a um subdomínio
// complementar do DNS VPS. Esta rotina nunca usa VPN_DNS_DOMAIN como fallback,
// nunca remove registros existentes e nunca toca em vpn.primecel.shop.
func (m *Manager) DNSVPSDomainsForIP(ctx context.Context, ip string) ([]string, error) {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return nil, errors.New("IP obrigatório")
	}
	if strings.TrimSpace(m.cloudflareToken(ctx)) == "" {
		return nil, errors.New("token Cloudflare vazio")
	}
	var active []string
	for _, domain := range AllowedDNSVPSDomainList {
		z, err := m.findZone(ctx, domain)
		if err != nil {
			continue
		}
		recs, err := m.listARecords(ctx, z.ID, domain)
		if err != nil {
			return active, err
		}
		for _, r := range recs {
			if r.Type == "A" && strings.EqualFold(r.Name, domain) && strings.TrimSpace(r.Content) == ip {
				active = append(active, domain)
				break
			}
		}
	}
	return active, nil
}

func (m *Manager) EnsureDNSVPSIP(ctx context.Context, domain string, ip string, dryRun bool) (SyncReport, error) {
	_ = m
	_ = ctx
	return SyncReport{Domain: strings.TrimSpace(domain), DesiredIPs: []string{strings.TrimSpace(ip)}, DryRun: dryRun}, errors.New("DNS VPS foi removido do bot")
}

func (m *Manager) SyncVPNDNS(ctx context.Context, domain string, ips []string, dryRun bool) (SyncReport, error) {
	_ = m
	_ = ctx
	_ = ips
	return SyncReport{Domain: strings.TrimSpace(domain), DryRun: dryRun}, errors.New("DNS VPS foi removido do bot")
}

func (m *Manager) RemoveVPNDNSIP(ctx context.Context, domain string, ip string, dryRun bool) (SyncReport, error) {
	_ = m
	_ = ctx
	return SyncReport{Domain: strings.TrimSpace(domain), DesiredIPs: []string{strings.TrimSpace(ip)}, DryRun: dryRun}, errors.New("DNS VPS foi removido do bot")
}

func (m *Manager) RemoveVPNDNS(ctx context.Context, domain string, dryRun bool) (SyncReport, error) {
	_ = m
	_ = ctx
	return SyncReport{Domain: strings.TrimSpace(domain), DryRun: dryRun}, errors.New("DNS VPS foi removido do bot")
}

func (m *Manager) cloudflareToken(ctx context.Context) string {
	if v, _ := m.Store.GetSetting(ctx, "cloudflare_token"); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(m.Config.CloudflareToken)
}

func (m *Manager) DesiredVPNIPs(ctx context.Context, includePrincipal bool) ([]string, error) {
	var ips []string
	if includePrincipal && strings.TrimSpace(m.Config.ServerHost) != "" {
		ips = append(ips, m.Config.ServerHost)
	}
	servers, err := m.Store.ListServers(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Enabled && strings.TrimSpace(s.Host) != "" {
			ips = append(ips, s.Host)
		}
	}
	return uniqIPs(ips), nil
}

func (m *Manager) ConfigureCheckUserDNS(ctx context.Context, fqdn, targetIP string, dryRun bool) (SyncReport, error) {
	fqdn = strings.Trim(strings.ToLower(fqdn), ". ")
	targetIP = strings.TrimSpace(targetIP)
	if IsReservedServerDomain(fqdn) || IsAllowedDNSVPSDomain(fqdn) {
		return SyncReport{Domain: fqdn, DesiredIPs: []string{targetIP}, DryRun: dryRun}, errors.New("checkuser não pode alterar domínios reservados dos servidores/DNS VPS")
	}
	if fqdn == "" || targetIP == "" {
		return SyncReport{}, errors.New("subdomínio e IP são obrigatórios")
	}
	rep := SyncReport{Domain: fqdn, DesiredIPs: []string{targetIP}, DryRun: dryRun}
	if dryRun {
		return rep, nil
	}
	if strings.TrimSpace(m.cloudflareToken(ctx)) == "" {
		return rep, errors.New("token Cloudflare vazio")
	}
	z, err := m.findZone(ctx, fqdn)
	if err != nil {
		return rep, err
	}
	rep.ZoneID = z.ID
	recs, err := m.listARecords(ctx, z.ID, fqdn)
	if err != nil {
		return rep, err
	}
	found := false
	for _, r := range recs {
		if strings.EqualFold(r.Name, fqdn) {
			if r.Content == targetIP {
				rep.Kept++
				found = true
				continue
			}
			// CheckUser também fica add-only: não remove/substitui registros
			// existentes. Se o IP desejado ainda não existir, cria um novo A.
		}
	}
	if !found {
		if err := m.createARecord(ctx, z.ID, fqdn, targetIP, true); err != nil {
			return rep, err
		}
		rep.Created++
	}
	b, _ := json.Marshal(rep)
	_ = m.Store.AddCloudflareEvent(ctx, "checkuser_dns", fqdn, true, string(b))
	return rep, nil
}

func (m *Manager) findZone(ctx context.Context, fqdn string) (zone, error) {
	parts := strings.Split(fqdn, ".")
	for i := 0; i < len(parts)-1; i++ {
		name := strings.Join(parts[i:], ".")
		zones, err := m.listZones(ctx, name)
		if err != nil {
			return zone{}, err
		}
		if len(zones) > 0 {
			return zones[0], nil
		}
	}
	return zone{}, fmt.Errorf("zona Cloudflare não encontrada para %s", fqdn)
}
func (m *Manager) listZones(ctx context.Context, name string) ([]zone, error) {
	var r cfResp
	if err := m.do(ctx, "GET", "/zones?name="+name, nil, &r); err != nil {
		return nil, err
	}
	var z []zone
	_ = json.Unmarshal(r.Result, &z)
	return z, nil
}
func (m *Manager) listARecords(ctx context.Context, zoneID, name string) ([]dnsRecord, error) {
	var r cfResp
	if err := m.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?type=A&name="+name, nil, &r); err != nil {
		return nil, err
	}
	var out []dnsRecord
	_ = json.Unmarshal(r.Result, &out)
	return out, nil
}
func (m *Manager) createARecord(ctx context.Context, zoneID, name, content string, proxied bool) error {
	body := map[string]any{"type": "A", "name": name, "content": content, "ttl": 1, "proxied": proxied}
	var r cfResp
	return m.do(ctx, "POST", "/zones/"+zoneID+"/dns_records", body, &r)
}

// deleteRecordForName é mantida apenas para compatibilidade defensiva.
// Regra v166: nenhuma rotina genérica do bot pode apagar registro DNS na
// Cloudflare. Isso evita que fluxo antigo de CheckUser, servidor, sincronização
// ou DNS VPS reaproveite um caminho de limpeza e apague vpn.primecel.shop ou
// qualquer outro subdomínio. A única exclusão permitida fica isolada em
// deleteDNSVPSRecordForName, chamada exclusivamente por RemoveVPNDNSIP após
// validação do subdomínio fixo e do IP escolhido manualmente.
func (m *Manager) deleteRecordForName(ctx context.Context, zoneID, id, recordName string) error {
	recordName = NormalizeDomain(recordName)
	payload := map[string]any{"record_id": id, "zone_id": zoneID, "record": recordName, "blocked": true}
	b, _ := json.Marshal(payload)
	kind := "cloudflare_delete_blocked"
	if IsReservedServerDomain(recordName) {
		kind = "cloudflare_delete_server_reserved_blocked"
	}
	_ = m.Store.AddCloudflareEvent(ctx, kind, recordName, true, string(b))
	return errors.New("remoção automática de DNS bloqueada pelo bot")
}

func (m *Manager) deleteServerDNSRecordForName(ctx context.Context, zoneID, id, recordName string) error {
	recordName = NormalizeDomain(recordName)
	if !IsDefaultServerDomain(recordName) {
		payload := map[string]any{"record_id": id, "zone_id": zoneID, "record": recordName, "blocked": true}
		b, _ := json.Marshal(payload)
		_ = m.Store.AddCloudflareEvent(ctx, "server_dns_delete_blocked", recordName, true, string(b))
		return errors.New("remoção automática permitida somente em vpn.primecel.shop")
	}
	var r cfResp
	return m.doRaw(ctx, "DELETE", "/zones/"+zoneID+"/dns_records/"+id, nil, &r)
}

// deleteDNSVPSRecordForName é o único caminho que executa DELETE na Cloudflare.
// Ele só é usado pelo botão manual 🗑️ Remover DNS e apenas para os três
// subdomínios fixos do DNS VPS. Mesmo aqui, vpn.primecel.shop é bloqueado.
func (m *Manager) deleteDNSVPSRecordForName(ctx context.Context, zoneID, id, recordName string) error {
	recordName = NormalizeDomain(recordName)
	if !IsAllowedDNSVPSDomain(recordName) || IsReservedServerDomain(recordName) {
		payload := map[string]any{"record_id": id, "zone_id": zoneID, "record": recordName, "blocked": true}
		b, _ := json.Marshal(payload)
		_ = m.Store.AddCloudflareEvent(ctx, "dns_vps_delete_blocked", recordName, true, string(b))
		return errors.New("remoção permitida somente nos DNS VPS fixos")
	}
	var r cfResp
	return m.doRaw(ctx, "DELETE", "/zones/"+zoneID+"/dns_records/"+id, nil, &r)
}

func isDangerousDNSWrite(method, path string) bool {
	m := strings.ToUpper(strings.TrimSpace(method))
	if !strings.Contains(path, "/dns_records") {
		return false
	}
	return m == "DELETE" || m == "PUT" || m == "PATCH"
}

func dnsWriteName(body any) string {
	if body == nil {
		return ""
	}
	if m, ok := body.(map[string]any); ok {
		if v, ok := m["name"].(string); ok {
			return NormalizeDomain(v)
		}
	}
	b, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err != nil {
		return ""
	}
	if v, ok := obj["name"].(string); ok {
		return NormalizeDomain(v)
	}
	return ""
}

func isForbiddenDNSCreate(method, path string, body any) (string, bool) {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m != "POST" || !strings.Contains(path, "/dns_records") {
		return "", false
	}
	name := dnsWriteName(body)
	if name == "" {
		return "", false
	}
	// O bot não pode mais criar nem reaproveitar domínios legados dos servidores.
	// Isso impede que qualquer config antiga ainda apontando para sv/server volte a
	// receber registros durante sincronização ou instalação.
	if IsLegacyServerDomain(name) || IsAllowedDNSVPSDomain(name) {
		return name, true
	}
	return "", false
}

func (m *Manager) do(ctx context.Context, method, path string, body any, out *cfResp) error {
	if isDangerousDNSWrite(method, path) {
		payload := map[string]any{"method": method, "path": path, "blocked": true}
		b, _ := json.Marshal(payload)
		_ = m.Store.AddCloudflareEvent(ctx, "cloudflare_dns_write_blocked", "", true, string(b))
		return errors.New("alteração destrutiva de DNS bloqueada; o bot só pode criar/adicionar registros")
	}
	if name, blocked := isForbiddenDNSCreate(method, path, body); blocked {
		payload := map[string]any{"method": method, "path": path, "name": name, "blocked": true}
		b, _ := json.Marshal(payload)
		_ = m.Store.AddCloudflareEvent(ctx, "cloudflare_dns_create_reserved_blocked", name, true, string(b))
		return fmt.Errorf("domínio reservado/legado bloqueado para criação automática: %s", name)
	}
	return m.doRaw(ctx, method, path, body, out)
}

func (m *Manager) doRaw(ctx context.Context, method, path string, body any, out *cfResp) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://api.cloudflare.com/client/v4"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.cloudflareToken(ctx))
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("cloudflare http %d: %s", resp.StatusCode, string(b))
	}
	if err := json.Unmarshal(b, out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("cloudflare api erro: %v", out.Errors)
	}
	return nil
}
func uniqIPs(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, ip := range in {
		ip = strings.TrimSpace(ip)
		if ip == "" || seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

var _ = model.Server{}
