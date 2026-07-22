// sharewifi provides a small web console for a Linux hostapd/dnsmasq hotspot.
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

//go:embed web.html
var webFS embed.FS

type Config struct {
	// Service settings are deliberately not part of the web/config-file model.
	// They are supplied at process start and remain in server memory only.
	Listen            string `json:"-"`
	Workdir           string `json:"-"`
	ConsoleUsername   string `json:"-"`
	ConsolePassword   string `json:"-"`
	Interface         string `json:"interface"`
	SSID              string `json:"ssid"`
	Passphrase        string `json:"passphrase"`
	CountryCode       string `json:"country_code"`
	Band              string `json:"band"`
	Channel           int    `json:"channel"`
	GatewayCIDR       string `json:"gateway_cidr"`
	DHCPStart         string `json:"dhcp_start"`
	DHCPEnd           string `json:"dhcp_end"`
	LeaseTime         string `json:"lease_time"`
	UpstreamInterface string `json:"upstream_interface"`
}

type Check struct {
	Name     string `json:"name"`
	Present  bool   `json:"present"`
	Required bool   `json:"required"`
	Help     string `json:"help"`
}
type Status struct {
	Running  bool    `json:"running"`
	Message  string  `json:"message"`
	Config   *Config `json:"config,omitempty"`
	Workdir  string  `json:"-"`
	Checks   []Check `json:"checks"`
	Firewall string  `json:"firewall"`
	Logs     string  `json:"logs,omitempty"`
}
type ClientStatus struct {
	Clients   []ClientTraffic  `json:"clients"`
	Interface InterfaceTraffic `json:"interface"`
	Error     string           `json:"error,omitempty"`
}
type ClientTraffic struct {
	MAC     string  `json:"mac"`
	IP      string  `json:"ip,omitempty"`
	Name    string  `json:"name,omitempty"`
	Signal  string  `json:"signal,omitempty"`
	TXRate  string  `json:"tx_rate,omitempty"`
	RXRate  string  `json:"rx_rate,omitempty"`
	TXBytes uint64  `json:"tx_bytes"`
	RXBytes uint64  `json:"rx_bytes"`
	TXBPS   float64 `json:"tx_bps"`
	RXBPS   float64 `json:"rx_bps"`
}
type InterfaceTraffic struct {
	RXBytes uint64  `json:"rx_bytes"`
	TXBytes uint64  `json:"tx_bytes"`
	RXBPS   float64 `json:"rx_bps"`
	TXBPS   float64 `json:"tx_bps"`
}
type trafficSample struct {
	rx, tx uint64
	at     time.Time
}
type app struct {
	mu               sync.Mutex
	workdir          string
	checks           []Check
	fw               string
	running          bool
	cfg              *Config
	hostapd, dnsmasq *exec.Cmd
	nmManaged        bool
	nmInterface      string
	oldForward       string
	lastError        string
	stopping         bool
	baseConfig       Config
	clientSamples    map[string]trafficSample
	interfaceSample  trafficSample
}

func main() {
	var listen, workdir, configPath, username, password string
	flag.StringVar(&listen, "listen", "0.0.0.0:8080", "web listen address")
	flag.StringVar(&workdir, "workdir", "", "runtime directory")
	flag.StringVar(&username, "username", "", "console basic-auth username")
	flag.StringVar(&password, "password", "", "console basic-auth password")
	flag.StringVar(&configPath, "config", "", "JSON configuration to start")
	flag.Parse()
	if configPath != "" {
		flag.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "config", "listen", "workdir", "username", "password":
				// These process-level settings are intentionally not stored in JSON.
			default:
				log.Fatalf("--config cannot be combined with --%s", f.Name)
			}
		})
	}
	if (username == "") != (password == "") {
		log.Fatal("--username and --password must be supplied together")
	}
	if runtime.GOOS != "linux" {
		log.Fatal("sharewifi only supports Linux")
	}
	if os.Geteuid() != 0 {
		log.Fatal("root is required: sudo sharewifi")
	}
	if workdir == "" {
		var err error
		workdir, err = os.MkdirTemp("", "sharewifi-")
		if err != nil {
			log.Fatal(err)
		}
	} else if err := os.MkdirAll(workdir, 0700); err != nil {
		log.Fatal(err)
	}
	// Keep the server-side defaults in sync with the embedded form.  The status
	// endpoint is also the source of truth used to populate the form, so it
	// must never publish an otherwise-valid configuration with empty defaults.
	a := &app{workdir: workdir, clientSamples: make(map[string]trafficSample), baseConfig: Config{
		Listen:          listen,
		Workdir:         workdir,
		ConsoleUsername: username,
		ConsolePassword: password,
		SSID:            "ShareWiFi",
		Passphrase:      "change-me-123",
		CountryCode:     "CN",
		Band:            "2.4GHz",
		Channel:         6,
		GatewayCIDR:     "192.168.50.1/24",
		DHCPStart:       "192.168.50.20",
		DHCPEnd:         "192.168.50.200",
		LeaseTime:       "12h",
	}}
	a.checks, a.fw = environment()
	if configPath != "" {
		cfg, err := readConfig(configPath)
		if err != nil {
			log.Fatal(err)
		}
		if err = a.start(cfg); err != nil {
			log.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	a.routes(mux)
	handler := basicAuth(mux, username, password)
	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if username == "" {
			log.Printf("WARNING: console has no authentication; listening on http://%s", listen)
		} else {
			log.Printf("console basic authentication enabled; listening on http://%s", listen)
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("web server: %v", err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	a.stop()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func basicAuth(next http.Handler, username, password string) http.Handler {
	if username == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		validUser := subtle.ConstantTimeCompare([]byte(u), []byte(username)) == 1
		validPassword := subtle.ConstantTimeCompare([]byte(p), []byte(password)) == 1
		if !ok || !validUser || !validPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="ShareWiFi"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *app) routes(m *http.ServeMux) {
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, _ := webFS.ReadFile("web.html")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	m.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		jsonOut(w, Status{Running: a.running, Message: statusMessage(a.running, a.lastError), Config: a.exportConfig(), Workdir: a.workdir, Checks: a.checks, Firewall: a.fw, Logs: a.logs()})
	})
	m.HandleFunc("/api/interfaces", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, wirelessInterfaces()) })
	m.HandleFunc("/api/clients", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, a.clients()) })
	m.HandleFunc("/api/config", a.configAPI)
	m.HandleFunc("/api/start", a.startAPI)
	m.HandleFunc("/api/stop", a.stopAPI)
}
func statusMessage(r bool, lastError string) string {
	if r {
		return "Wi-Fi sharing is running"
	}
	if lastError != "" {
		return "Wi-Fi sharing failed: " + lastError
	}
	return "Wi-Fi sharing is stopped"
}
func jsonOut(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
func jsonErr(w http.ResponseWriter, code int, e error) {
	w.WriteHeader(code)
	jsonOut(w, map[string]string{"error": e.Error()})
}

func (a *app) configAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		jsonOut(w, a.exportConfig())
		return
	}
	cfg, err := decodeConfig(r.Body)
	if err != nil {
		jsonErr(w, 400, err)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=sharewifi.json")
	jsonOut(w, cfg)
}
func (a *app) startAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, 405, errors.New("POST required"))
		return
	}
	cfg, err := decodeConfig(r.Body)
	if err == nil {
		err = a.start(cfg)
	}
	if err != nil {
		log.Printf("hotspot start request rejected: %v", err)
		jsonErr(w, 400, err)
		return
	}
	jsonOut(w, map[string]string{"message": "started"})
}
func (a *app) stopAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, 405, errors.New("POST required"))
		return
	}
	a.stop()
	jsonOut(w, map[string]string{"message": "stopped"})
}

func environment() ([]Check, string) {
	cmds := []struct {
		name     string
		required bool
	}{{"hostapd", true}, {"dnsmasq", true}, {"ip", true}, {"iw", true}, {"hostapd_cli", false}}
	checks := make([]Check, 0, len(cmds)+1)
	for _, c := range cmds {
		_, e := exec.LookPath(c.name)
		checks = append(checks, Check{c.name, e == nil, c.required, installHelp(c.name)})
	}
	_, nft := exec.LookPath("nft")
	_, ipt := exec.LookPath("iptables")
	fw := ""
	if nft == nil {
		fw = "nftables"
	} else if ipt == nil {
		fw = "iptables"
	}
	checks = append(checks, Check{"nft or iptables", fw != "", true, "Debian/Ubuntu: sudo apt install nftables; Fedora/CentOS: sudo dnf install nftables"})
	return checks, fw
}
func installHelp(name string) string {
	pkg := name
	if name == "ip" {
		pkg = "iproute2"
	}
	return "Debian/Ubuntu: sudo apt install " + pkg + "; Fedora/CentOS: sudo dnf install " + pkg
}
func missingChecks(c []Check) error {
	var x []string
	for _, v := range c {
		if v.Required && !v.Present {
			x = append(x, v.Name+" ("+v.Help+")")
		}
	}
	if len(x) > 0 {
		return errors.New("missing required programs: " + strings.Join(x, "; "))
	}
	return nil
}

func decodeConfig(r io.Reader) (Config, error) {
	var c Config
	d := json.NewDecoder(io.LimitReader(r, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return c, errors.New("configuration must contain exactly one JSON object")
	}
	return c, validate(c)
}
func readConfig(p string) (Config, error) {
	f, e := os.Open(p)
	if e != nil {
		return Config{}, e
	}
	defer f.Close()
	return decodeConfig(f)
}
func validate(c Config) error {
	if c.Listen == "" {
		c.Listen = "0.0.0.0:8080"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return errors.New("listen must be an address such as 0.0.0.0:8080")
	}
	if (c.ConsoleUsername == "") != (c.ConsolePassword == "") {
		return errors.New("console username and password must both be set or both be empty")
	}
	if hasControl(c.ConsoleUsername) || hasControl(c.ConsolePassword) {
		return errors.New("console credentials cannot contain control characters")
	}
	if strings.TrimSpace(c.Interface) == "" || strings.ContainsAny(c.Interface, " /\t\n") {
		return errors.New("a valid wireless interface is required")
	}
	if len(c.SSID) < 1 || len(c.SSID) > 32 || hasControl(c.SSID) {
		return errors.New("SSID must be 1-32 characters")
	}
	if len(c.Passphrase) < 8 || len(c.Passphrase) > 63 || hasControl(c.Passphrase) {
		return errors.New("password must be 8-63 characters")
	}
	if len(c.CountryCode) != 2 || !asciiLetters(c.CountryCode) {
		return errors.New("country code must have two letters")
	}
	c.CountryCode = strings.ToUpper(c.CountryCode)
	if c.Band != "2.4GHz" && c.Band != "5GHz" {
		return errors.New("band must be 2.4GHz or 5GHz")
	}
	if c.Channel < 1 || c.Channel > 196 {
		return errors.New("invalid channel")
	}
	ip, n, e := net.ParseCIDR(c.GatewayCIDR)
	ones, bits := 0, 0
	if n != nil {
		ones, bits = n.Mask.Size()
	}
	if e != nil || ip.To4() == nil || bits != 32 || ones < 1 || ones > 30 {
		return errors.New("gateway must be an IPv4 CIDR")
	}
	s := net.ParseIP(c.DHCPStart).To4()
	end := net.ParseIP(c.DHCPEnd).To4()
	if s == nil || end == nil || !n.Contains(s) || !n.Contains(end) || bytesCompare(s, end) > 0 {
		return errors.New("DHCP range must be ordered IPv4 addresses inside gateway subnet")
	}
	if _, e = time.ParseDuration(c.LeaseTime); e != nil {
		return errors.New("lease time must be a Go duration such as 12h")
	}
	if c.UpstreamInterface != "" && strings.ContainsAny(c.UpstreamInterface, " /\t\n") {
		return errors.New("invalid upstream interface")
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
func asciiLetters(s string) bool {
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}
func bytesCompare(a, b net.IP) int {
	for i := range a {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func (a *app) start(c Config) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return errors.New("Wi-Fi sharing is already running")
	}
	a.lastError = ""
	a.clientSamples = make(map[string]trafficSample)
	a.interfaceSample = trafficSample{}
	if err := a.prepareLogs(); err != nil {
		return err
	}
	if err := missingChecks(a.checks); err != nil {
		return err
	}
	if !contains(wirelessInterfaces(), c.Interface) {
		return fmt.Errorf("%s is not an available wireless interface", c.Interface)
	}
	if !wirelessAPCapable(c.Interface) {
		return fmt.Errorf("%s does not report AP mode support", c.Interface)
	}
	upstream := c.UpstreamInterface
	if upstream == "" {
		var e error
		upstream, e = defaultInterface()
		if e != nil {
			return e
		}
		c.UpstreamInterface = upstream
	}
	if upstream == c.Interface {
		return errors.New("upstream interface cannot be the AP interface")
	}
	if err := a.unmanageNM(c.Interface); err != nil {
		return err
	}
	a.cfg = &c
	rollback := func(e error) error { a.lastError = e.Error(); a.cleanupLocked(); return e }
	if err := run("ip", "link", "set", c.Interface, "down"); err != nil {
		return rollback(err)
	}
	if err := run("ip", "addr", "flush", "dev", c.Interface); err != nil {
		return rollback(err)
	}
	if err := run("ip", "addr", "add", c.GatewayCIDR, "dev", c.Interface); err != nil {
		return rollback(err)
	}
	if err := run("ip", "link", "set", c.Interface, "up"); err != nil {
		return rollback(err)
	}
	a.oldForward = readTrim("/proc/sys/net/ipv4/ip_forward")
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0644); err != nil {
		return rollback(err)
	}
	if err := a.addFirewall(c); err != nil {
		return rollback(err)
	}
	if err := a.writeConfigs(c); err != nil {
		return rollback(err)
	}
	hostLog, err := os.OpenFile(filepath.Join(a.workdir, "hostapd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return rollback(err)
	}
	a.hostapd = exec.Command("hostapd", filepath.Join(a.workdir, "hostapd.conf"))
	a.hostapd.Stdout = hostLog
	a.hostapd.Stderr = hostLog
	if err := a.hostapd.Start(); err != nil {
		_ = hostLog.Close()
		return rollback(fmt.Errorf("hostapd: %w", err))
	}
	hostapdCmd := a.hostapd
	go func(cmd *exec.Cmd) {
		err := cmd.Wait()
		_ = hostLog.Close()
		if err != nil {
			a.recordFailure("hostapd", err)
		}
	}(hostapdCmd)
	if err := a.processSurvives(hostapdCmd, "hostapd"); err != nil {
		return rollback(err)
	}
	dnsLog, err := os.OpenFile(filepath.Join(a.workdir, "dnsmasq.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return rollback(err)
	}
	a.dnsmasq = exec.Command("dnsmasq", "--keep-in-foreground", "--conf-file="+filepath.Join(a.workdir, "dnsmasq.conf"))
	a.dnsmasq.Stdout = dnsLog
	a.dnsmasq.Stderr = dnsLog
	if err := a.dnsmasq.Start(); err != nil {
		_ = dnsLog.Close()
		return rollback(fmt.Errorf("dnsmasq: %w", err))
	}
	dnsmasqCmd := a.dnsmasq
	go func(cmd *exec.Cmd) {
		err := cmd.Wait()
		_ = dnsLog.Close()
		if err != nil {
			a.recordFailure("dnsmasq", err)
		}
	}(dnsmasqCmd)
	if err := a.processSurvives(dnsmasqCmd, "dnsmasq"); err != nil {
		return rollback(err)
	}
	a.running = true
	return nil
}
func (a *app) stop() { a.mu.Lock(); defer a.mu.Unlock(); a.cleanupLocked() }
func (a *app) cleanupLocked() {
	a.stopping = true
	if a.dnsmasq != nil && a.dnsmasq.Process != nil {
		_ = a.dnsmasq.Process.Signal(syscall.SIGTERM)
	}
	if a.hostapd != nil && a.hostapd.Process != nil {
		_ = a.hostapd.Process.Signal(syscall.SIGTERM)
	}
	a.dnsmasq = nil
	a.hostapd = nil
	if a.cfg != nil {
		a.removeFirewall(*a.cfg)
		if a.oldForward != "" {
			_ = os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(a.oldForward+"\n"), 0644)
		}
		a.restoreNM()
	}
	a.running = false
	a.cfg = nil
	a.clientSamples = make(map[string]trafficSample)
	a.interfaceSample = trafficSample{}
	a.stopping = false
}
func (a *app) prepareLogs() error {
	for _, name := range []string{"hostapd.log", "dnsmasq.log"} {
		if err := os.WriteFile(filepath.Join(a.workdir, name), nil, 0600); err != nil {
			return err
		}
	}
	return nil
}
func (a *app) processSurvives(cmd *exec.Cmd, name string) error {
	time.Sleep(350 * time.Millisecond)
	if cmd.ProcessState != nil {
		return fmt.Errorf("%s exited during startup; see logs below", name)
	}
	return nil
}
func (a *app) recordFailure(name string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopping {
		return
	}
	a.running = false
	a.lastError = fmt.Sprintf("%s exited: %v", name, err)
}
func (a *app) logs() string {
	var parts []string
	for _, name := range []string{"hostapd.log", "dnsmasq.log"} {
		if b, err := os.ReadFile(filepath.Join(a.workdir, name)); err == nil && len(b) > 0 {
			if len(b) > 8192 {
				b = b[len(b)-8192:]
			}
			parts = append(parts, "["+name+"]\n"+string(b))
		}
	}
	return strings.Join(parts, "\n")
}
func (a *app) exportConfig() *Config {
	if a.cfg == nil {
		c := a.baseConfig
		return &c
	}
	c := *a.cfg
	c.Workdir = a.workdir
	return &c
}
func (a *app) writeConfigs(c Config) error {
	h := fmt.Sprintf("interface=%s\ndriver=nl80211\nctrl_interface=%s\nctrl_interface_group=0\nssid=%s\nhw_mode=%s\nchannel=%d\ncountry_code=%s\nieee80211d=1\nwmm_enabled=1\nauth_algs=1\nwpa=2\nwpa_passphrase=%s\nwpa_key_mgmt=WPA-PSK\nrsn_pairwise=CCMP\n", c.Interface, a.workdir, c.SSID, map[bool]string{true: "g", false: "a"}[c.Band == "2.4GHz"], c.Channel, c.CountryCode, c.Passphrase)
	// The host may already run systemd-resolved, NetworkManager dnsmasq, or
	// another DNS daemon on port 53.  This instance is only the DHCP authority;
	// disable its DNS listener and pass upstream resolvers to clients instead.
	d := fmt.Sprintf("interface=%s\nbind-interfaces\nport=0\ndhcp-leasefile=%s\ndhcp-range=%s,%s,%s\ndhcp-option=3,%s\ndhcp-option=6,%s\n", c.Interface, filepath.Join(a.workdir, "dnsmasq.leases"), c.DHCPStart, c.DHCPEnd, c.LeaseTime, hostIP(c.GatewayCIDR), strings.Join(upstreamDNS(), ","))
	if err := os.WriteFile(filepath.Join(a.workdir, "hostapd.conf"), []byte(h), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.workdir, "dnsmasq.conf"), []byte(d), 0600)
}
func upstreamDNS() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		var out []string
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "nameserver" {
				ip := net.ParseIP(f[1])
				if ip != nil && !ip.IsLoopback() {
					out = append(out, f[1])
				}
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	// Domestic public resolvers are the fallback when the host does not expose
	// a usable upstream resolver (for example, resolv.conf only has 127.0.0.53).
	return []string{"223.5.5.5", "114.114.114.114"}
}
func hostIP(c string) string { i, _, _ := net.ParseCIDR(c); return i.String() }
func (a *app) unmanageNM(iface string) error {
	if _, e := exec.LookPath("nmcli"); e != nil {
		return nil
	}
	out, e := exec.Command("nmcli", "-t", "-f", "GENERAL.STATE", "device", "show", iface).Output()
	if e != nil {
		return nil
	}
	if !strings.Contains(string(out), "unmanaged") {
		if e = run("nmcli", "device", "set", iface, "managed", "no"); e != nil {
			return fmt.Errorf("NetworkManager: %w", e)
		}
		a.nmManaged = true
		a.nmInterface = iface
	}
	return nil
}
func (a *app) restoreNM() {
	if a.nmManaged {
		_ = run("nmcli", "device", "set", a.nmInterface, "managed", "yes")
	}
	a.nmManaged = false
	a.nmInterface = ""
}
func (a *app) addFirewall(c Config) error {
	subnet := subnet(c.GatewayCIDR)
	if a.fw == "nftables" {
		// nft requires a statement separator between chain declarations.  Use a
		// multiline ruleset rather than compacting it into one line: besides being
		// valid for nft, it makes an error printed by nft directly actionable.
		script := fmt.Sprintf(`table ip sharewifi {
  chain forward {
    type filter hook forward priority 0;
    iifname "%s" oifname "%s" accept
    iifname "%s" oifname "%s" ct state established,related accept
  }
  chain postrouting {
    type nat hook postrouting priority 100;
    oifname "%s" ip saddr %s masquerade
  }
}
`, c.Interface, c.UpstreamInterface, c.UpstreamInterface, c.Interface, c.UpstreamInterface, subnet)
		return runInput(script, "nft", "-f", "-")
	}
	if err := run("iptables", "-A", "FORWARD", "-i", c.Interface, "-o", c.UpstreamInterface, "-j", "ACCEPT"); err != nil {
		return err
	}
	if err := run("iptables", "-A", "FORWARD", "-i", c.UpstreamInterface, "-o", c.Interface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
		return err
	}
	return run("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", subnet, "-o", c.UpstreamInterface, "-j", "MASQUERADE")
}
func (a *app) removeFirewall(c Config) {
	if a.fw == "nftables" {
		_ = run("nft", "delete", "table", "ip", "sharewifi")
	} else {
		_ = run("iptables", "-D", "FORWARD", "-i", c.Interface, "-o", c.UpstreamInterface, "-j", "ACCEPT")
		_ = run("iptables", "-D", "FORWARD", "-i", c.UpstreamInterface, "-o", c.Interface, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT")
		_ = run("iptables", "-t", "nat", "-D", "POSTROUTING", "-s", subnet(c.GatewayCIDR), "-o", c.UpstreamInterface, "-j", "MASQUERADE")
	}
}
func subnet(c string) string { _, n, _ := net.ParseCIDR(c); return n.String() }
func (a *app) clients() ClientStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.running || a.cfg == nil {
		return ClientStatus{}
	}
	now := time.Now()
	out, e := exec.Command("hostapd_cli", "-p", a.workdir, "-i", a.cfg.Interface, "all_sta").CombinedOutput()
	if e != nil {
		return ClientStatus{Error: strings.TrimSpace(string(out))}
	}
	clients := parseStationTraffic(string(out))
	leases := readLeases(filepath.Join(a.workdir, "dnsmasq.leases"))
	for i := range clients {
		if lease, ok := leases[strings.ToLower(clients[i].MAC)]; ok {
			clients[i].IP = lease.ip
			clients[i].Name = lease.name
		}
		previous, ok := a.clientSamples[clients[i].MAC]
		clients[i].RXBPS = bytesPerSecond(clients[i].RXBytes, previous.rx, previous.at, now, ok)
		clients[i].TXBPS = bytesPerSecond(clients[i].TXBytes, previous.tx, previous.at, now, ok)
		a.clientSamples[clients[i].MAC] = trafficSample{rx: clients[i].RXBytes, tx: clients[i].TXBytes, at: now}
	}
	interfaceTraffic, e := readInterfaceTraffic(a.cfg.Interface)
	if e != nil {
		return ClientStatus{Clients: clients, Error: e.Error()}
	}
	interfaceTraffic.RXBPS = bytesPerSecond(interfaceTraffic.RXBytes, a.interfaceSample.rx, a.interfaceSample.at, now, !a.interfaceSample.at.IsZero())
	interfaceTraffic.TXBPS = bytesPerSecond(interfaceTraffic.TXBytes, a.interfaceSample.tx, a.interfaceSample.at, now, !a.interfaceSample.at.IsZero())
	a.interfaceSample = trafficSample{rx: interfaceTraffic.RXBytes, tx: interfaceTraffic.TXBytes, at: now}
	return ClientStatus{Clients: clients, Interface: interfaceTraffic}
}

type leaseInfo struct{ ip, name string }

func readLeases(path string) map[string]leaseInfo {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]leaseInfo{}
	}
	leases := make(map[string]leaseInfo)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		mac := strings.ToLower(fields[1])
		if _, err := net.ParseMAC(mac); err != nil {
			continue
		}
		name := fields[3]
		if name == "*" {
			name = ""
		}
		leases[mac] = leaseInfo{ip: fields[2], name: name}
	}
	return leases
}

func parseStationTraffic(out string) []ClientTraffic {
	var clients []ClientTraffic
	var current *ClientTraffic
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if mac, err := net.ParseMAC(line); err == nil && len(mac) == 6 {
			clients = append(clients, ClientTraffic{MAC: strings.ToUpper(line)})
			current = &clients[len(clients)-1]
			continue
		}
		if current == nil {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "signal":
			current.Signal = value + " dBm"
		case "tx_rate_info":
			current.TXRate = value
		case "rx_rate_info":
			current.RXRate = value
		case "tx_bytes":
			current.TXBytes, _ = strconv.ParseUint(value, 10, 64)
		case "rx_bytes":
			current.RXBytes, _ = strconv.ParseUint(value, 10, 64)
		}
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].MAC < clients[j].MAC })
	return clients
}

func readInterfaceTraffic(iface string) (InterfaceTraffic, error) {
	rx, err := readUint(filepath.Join("/sys/class/net", iface, "statistics/rx_bytes"))
	if err != nil {
		return InterfaceTraffic{}, err
	}
	tx, err := readUint(filepath.Join("/sys/class/net", iface, "statistics/tx_bytes"))
	if err != nil {
		return InterfaceTraffic{}, err
	}
	return InterfaceTraffic{RXBytes: rx, TXBytes: tx}, nil
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func bytesPerSecond(current, previous uint64, then, now time.Time, valid bool) float64 {
	if !valid || current < previous {
		return 0
	}
	seconds := now.Sub(then).Seconds()
	if seconds <= 0 {
		return 0
	}
	return float64(current-previous) / seconds
}
func run(name string, args ...string) error {
	o, e := exec.Command(name, args...).CombinedOutput()
	if e != nil {
		return fmt.Errorf("%s: %w: %s", name, e, strings.TrimSpace(string(o)))
	}
	return nil
}
func runInput(s, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdin = strings.NewReader(s)
	o, e := c.CombinedOutput()
	if e != nil {
		return fmt.Errorf("%s: %w: %s", name, e, strings.TrimSpace(string(o)))
	}
	return nil
}
func readTrim(p string) string { b, _ := os.ReadFile(p); return strings.TrimSpace(string(b)) }
func wirelessInterfaces() []string {
	o, e := exec.Command("iw", "dev").Output()
	if e != nil {
		return []string{}
	}
	var r []string
	for _, l := range strings.Split(string(o), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[0] == "Interface" {
			r = append(r, f[1])
		}
	}
	sort.Strings(r)
	return r
}
func wirelessAPCapable(iface string) bool {
	out, err := exec.Command("iw", "dev", iface, "info").Output()
	if err != nil {
		return false
	}
	var phy string
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "wiphy" {
			phy = "phy" + f[1]
			break
		}
	}
	if phy == "" {
		return false
	}
	out, err = exec.Command("iw", "phy", phy, "info").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "* AP\n") || strings.Contains(string(out), "* AP ")
}
func contains(a []string, s string) bool {
	for _, x := range a {
		if x == s {
			return true
		}
	}
	return false
}
func defaultInterface() (string, error) {
	o, e := exec.Command("ip", "route", "show", "default").Output()
	if e != nil {
		return "", e
	}
	f := strings.Fields(string(o))
	for i := range f {
		if f[i] == "dev" && i+1 < len(f) {
			return f[i+1], nil
		}
	}
	return "", errors.New("no default route found; select an upstream interface")
}
