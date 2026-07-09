package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/alidns"
	"github.com/cloudflare/cloudflare-go"
)

var defaultAPIURLs = []string{
	"https://checkip.amazonaws.com",
	"https://ipv4.icanhazip.com",
	"https://ip.3322.net",
	"https://ipinfo.io/json",
	"https://api.ipify.org?format=json",
}

type Config struct {
	DnsProvider         string   `json:"dnsProvider"`
	CloudflareToken     string   `json:"cloudflareToken"`
	ZoneID              string   `json:"zoneID"`
	AccessKey           string   `json:"accessKey"`
	AccessSecret        string   `json:"accessSecret"`
	DomainName          string   `json:"domainName"`
	LogPath             string   `json:"logPath"`
	APIURLs             []string `json:"apiURLs"`
	APIURL              string   `json:"apiURL"`
	RecordType          string   `json:"recordType"`
	RR                  string   `json:"rr"`
	CheckInterval       int      `json:"checkInterval"`
	IPCheckInterval     int      `json:"ipCheckInterval"`
	ForceUpdateInterval int      `json:"forceUpdateInterval"`
	ProbeTargets        []string `json:"probeTargets"`
	Timeout             int      `json:"timeout"`
	TTL                 int      `json:"ttl"`
	SMTPHost            string   `json:"smtpHost"`
	SMTPPort            int      `json:"smtpPort"`
	SMTPUser            string   `json:"smtpUser"`
	SMTPPassword        string   `json:"smtpPassword"`
	NotifyEmail         string   `json:"notifyEmail"`
}

var ErrNoUpdateNeeded = errors.New("DNS record unchanged, no update needed")

func getPublicIPWithFallback(apiURLs []string, timeout time.Duration) (string, error) {
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var lastErr error

	for _, url := range apiURLs {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s returned status %d", url, resp.StatusCode)
			continue
		}

		text := strings.TrimSpace(string(body))
		if text == "" {
			lastErr = fmt.Errorf("%s returned empty body", url)
			continue
		}

		var result map[string]interface{}
		if json.Unmarshal(body, &result) == nil {
			if ip, ok := result["ip"].(string); ok && net.ParseIP(ip) != nil {
				return ip, nil
			}
			if origin, ok := result["origin"].(string); ok && net.ParseIP(origin) != nil {
				return origin, nil
			}
		}

		if net.ParseIP(text) != nil {
			return text, nil
		}

		lastErr = fmt.Errorf("%s returned unrecognized format: %s", url, text)
	}

	if lastErr != nil {
		return "", fmt.Errorf("all IP APIs failed, last error: %v", lastErr)
	}
	return "", errors.New("all IP APIs failed")
}

func checkReachability(targets []string, timeout time.Duration) bool {
	for _, target := range targets {
		conn, err := net.DialTimeout("tcp", target, timeout)
		if err == nil {
			conn.Close()
			return true
		}
	}
	return false
}

func updateDNSRecord(client *alidns.Client, domainName, publicIP, recordType, rr string) error {
	describeRequest := alidns.CreateDescribeDomainRecordsRequest()
	describeRequest.Scheme = "https"
	describeRequest.DomainName = domainName
	describeRequest.SetReadTimeout(10 * time.Second)
	describeRequest.SetConnectTimeout(5 * time.Second)

	records, err := client.DescribeDomainRecords(describeRequest)
	if err != nil {
		return err
	}

	for _, record := range records.DomainRecords.Record {
		if record.Type == recordType && record.RR == rr {
			if record.Value == publicIP {
				return ErrNoUpdateNeeded
			}

			updateRequest := alidns.CreateUpdateDomainRecordRequest()
			updateRequest.Scheme = "https"
			updateRequest.RecordId = record.RecordId
			updateRequest.RR = record.RR
			updateRequest.Type = record.Type
			updateRequest.Value = publicIP
			updateRequest.SetReadTimeout(10 * time.Second)
			updateRequest.SetConnectTimeout(5 * time.Second)

			_, err := client.UpdateDomainRecord(updateRequest)
			return err
		}
	}

	addRequest := alidns.CreateAddDomainRecordRequest()
	addRequest.Scheme = "https"
	addRequest.DomainName = domainName
	addRequest.Type = recordType
	addRequest.RR = rr
	addRequest.Value = publicIP
	addRequest.SetReadTimeout(10 * time.Second)
	addRequest.SetConnectTimeout(5 * time.Second)

	_, err = client.AddDomainRecord(addRequest)
	return err
}

type backoffState struct {
	failCount    int
	lastFailTime time.Time
	backoff      int
}

func (b *backoffState) onSuccess() {
	b.failCount = 0
	b.backoff = 0
}

func (b *backoffState) onFailure() {
	b.failCount++
	b.backoff = calcBackoff(b.failCount)
	b.lastFailTime = time.Now()
}

func (b *backoffState) shouldSkip() bool {
	return b.backoff > 0 && time.Since(b.lastFailTime) < time.Duration(b.backoff)*time.Second
}

func main() {
	configFilePath := flag.String("config", "config.json", "Path to the configuration file")
	flag.Parse()

	if _, err := os.Stat(*configFilePath); os.IsNotExist(err) {
		saveDefaultConfig(*configFilePath)
		fmt.Printf("Default configuration file '%s' created. Please edit it with your credentials and domain name.\n", *configFilePath)
		os.Exit(0)
	}

	config, err := loadConfig(*configFilePath)
	if err != nil {
		log.Fatal("Failed to load configuration:", err)
	}

	applyConfigDefaults(&config)

	logPath := config.LogPath
	if logPath == "" {
		logPath = "DDns.log"
	}

	logDir := filepath.Dir(logPath)
	if logDir != "." && logDir != "" {
		if err := os.MkdirAll(logDir, 0700); err != nil {
			log.Fatal("Failed to create log directory:", err)
		}
	}

	logFile, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}
	defer logFile.Close()

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	logger := log.New(multiWriter, "DDns: ", log.LstdFlags|log.Lmicroseconds)

	defer func() {
		if r := recover(); r != nil {
			logger.Printf("PANIC recovered: %v\nstack: %s", r, debug.Stack())
			os.Exit(1)
		}
	}()

	var client *alidns.Client
	if config.DnsProvider != "cloudflare" {
		var err error
		client, err = alidns.NewClientWithAccessKey("cn-hangzhou", config.AccessKey, config.AccessSecret)
		if err != nil {
			logger.Fatalf("Failed to create Aliyun DNS client: %v", err)
		}
	}

	apiURLs := config.APIURLs
	if len(apiURLs) == 0 {
		apiURLs = defaultAPIURLs
	}

	httpTimeout := time.Duration(config.Timeout) * time.Second
	probeTimeout := 3 * time.Second

	lastKnownIP := ""
	wasReachable := true

	if checkReachability(config.ProbeTargets, probeTimeout) {
		ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
		if ip != "" {
			logger.Printf("Initial public IP: %s, updating DNS records...", ip)
			lastKnownIP = updateAllRRs(logger, client, config, ip)
		}
	} else {
		logger.Println("Network unreachable at startup, waiting for recovery...")
	}

	probeTicker := time.NewTicker(time.Duration(config.CheckInterval) * time.Second)
	ipCheckTicker := time.NewTicker(time.Duration(config.IPCheckInterval) * time.Second)
	forceTicker := time.NewTicker(time.Duration(config.ForceUpdateInterval) * time.Minute)
	defer func() {
		probeTicker.Stop()
		ipCheckTicker.Stop()
		forceTicker.Stop()
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	logger.Printf("DDNS started, domain: %s, rr: %s, checkInterval: %ds, ipCheckInterval: %ds, forceUpdateInterval: %dm",
		config.DomainName, config.RR, config.CheckInterval, config.IPCheckInterval, config.ForceUpdateInterval)

	bo := &backoffState{}

	for {
		select {
		case <-probeTicker.C:
			reachable := checkReachability(config.ProbeTargets, probeTimeout)
			if !wasReachable && reachable {
				logger.Println("Network recovered, checking IP immediately...")
				ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
				if ip != "" && ip != lastKnownIP {
					lastKnownIP = applyUpdate(logger, bo, client, config, ip)
				} else if ip != "" {
					logger.Printf("IP unchanged: %s", ip)
				}
			}
			wasReachable = reachable

		case <-ipCheckTicker.C:
			if bo.shouldSkip() {
				continue
			}
			ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
			if ip != "" && ip != lastKnownIP {
				logger.Printf("IP changed: %s -> %s", lastKnownIP, ip)
				lastKnownIP = applyUpdate(logger, bo, client, config, ip)
			}

		case <-forceTicker.C:
			ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
			if ip != "" {
				if ip != lastKnownIP {
					logger.Printf("Force update: IP changed from %s to %s", lastKnownIP, ip)
				}
				lastKnownIP = applyUpdate(logger, bo, client, config, ip)
			}

		case sig := <-sigChan:
			logger.Printf("Received signal %v, shutting down...", sig)
			return
		}
	}
}

func applyUpdate(logger *log.Logger, bo *backoffState, client *alidns.Client, config Config, ip string) string {
	result := updateAllRRs(logger, client, config, ip)
	if result != "" {
		bo.onSuccess()
		return result
	}
	bo.onFailure()
	logger.Printf("DNS update failed, retry in %ds (fail #%d)", bo.backoff, bo.failCount)
	return ""
}

func fetchAndLogIP(logger *log.Logger, apiURLs []string, timeout time.Duration) string {
	ip, err := getPublicIPWithFallback(apiURLs, timeout)
	if err != nil {
		logger.Printf("Failed to get public IP: %v", err)
		return ""
	}
	return ip
}

func updateAllRRs(logger *log.Logger, client *alidns.Client, config Config, publicIP string) string {
	rrs := strings.Split(config.RR, ",")
	success := true
	updated := false

	var cfAPI *cloudflare.API
	if config.DnsProvider == "cloudflare" {
		var err error
		cfAPI, err = cloudflare.NewWithAPIToken(config.CloudflareToken)
		if err != nil {
			logger.Printf("Failed to create Cloudflare client: %v", err)
			return ""
		}
	}

	for _, r := range rrs {
		currentRR := strings.TrimSpace(r)
		if currentRR == "" {
			continue
		}
		var err error
		if config.DnsProvider == "cloudflare" {
			err = updateDNSRecordCloudflare(cfAPI, config, publicIP, currentRR)
		} else {
			err = updateDNSRecord(client, config.DomainName, publicIP, config.RecordType, currentRR)
		}
		if err != nil {
			if err == ErrNoUpdateNeeded {
				logger.Printf("RR '%s' already points to %s, skipped", currentRR, publicIP)
			} else {
				logger.Printf("RR '%s' update failed: %v", currentRR, err)
				success = false
			}
		} else {
			logger.Printf("RR '%s' updated successfully to %s", currentRR, publicIP)
			updated = true
		}
	}
	if success {
		if updated {
			if err := sendNotify(config, publicIP); err != nil {
				logger.Printf("Failed to send notification: %v", err)
			}
		}
		return publicIP
	}
	return ""
}

func updateDNSRecordCloudflare(cfAPI *cloudflare.API, config Config, publicIP, rr string) error {
	ctx := context.Background()

	recordName := rr
	if rr == "@" {
		recordName = config.DomainName
	} else {
		recordName = rr + "." + config.DomainName
	}

	records, _, err := cfAPI.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(config.ZoneID), cloudflare.ListDNSRecordsParams{
		Type: config.RecordType,
		Name: recordName,
	})
	if err != nil {
		return err
	}

	for _, rec := range records {
		if rec.Type == config.RecordType && rec.Name == recordName {
			if rec.Content == publicIP {
				return ErrNoUpdateNeeded
			}
			_, err := cfAPI.UpdateDNSRecord(ctx, cloudflare.ZoneIdentifier(config.ZoneID), cloudflare.UpdateDNSRecordParams{
				ID:      rec.ID,
				Type:    config.RecordType,
				Name:    recordName,
				Content: publicIP,
				TTL:     config.TTL,
				Proxied: cloudflare.BoolPtr(false),
			})
			return err
		}
	}

	_, err = cfAPI.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(config.ZoneID), cloudflare.CreateDNSRecordParams{
		Type:    config.RecordType,
		Name:    recordName,
		Content: publicIP,
		TTL:     config.TTL,
		Proxied: cloudflare.BoolPtr(false),
	})
	return err
}

func sendNotify(config Config, publicIP string) error {
	if config.SMTPHost == "" || config.NotifyEmail == "" {
		return nil
	}
	auth := smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, config.SMTPHost)
	subject := fmt.Sprintf("[DDNS] %s -> %s", config.DomainName, publicIP)
	body := fmt.Sprintf("Domain: %s\r\nIP: %s\r\nTime: %s\r\nRR: %s",
		config.DomainName, publicIP, time.Now().Format("2006-01-02 15:04:05"), config.RR)
	msg := []byte("To: " + config.NotifyEmail + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"Content-Type: text/plain; charset=UTF-8\r\n" +
		"\r\n" + body)
	addr := fmt.Sprintf("%s:%d", config.SMTPHost, config.SMTPPort)
	return smtp.SendMail(addr, auth, config.SMTPUser, []string{config.NotifyEmail}, msg)
}

func calcBackoff(failCount int) int {
	switch {
	case failCount <= 1:
		return 30
	case failCount == 2:
		return 60
	case failCount == 3:
		return 120
	case failCount == 4:
		return 300
	default:
		return 600
	}
}

func loadConfig(filePath string) (Config, error) {
	var config Config
	file, err := os.Open(filePath)
	if err != nil {
		return config, err
	}
	defer file.Close()

	decoder := json.NewDecoder(file)
	err = decoder.Decode(&config)
	if err != nil {
		return config, err
	}

	if config.APIURL != "" {
		config.APIURLs = append([]string{config.APIURL}, config.APIURLs...)
	}

	return config, nil
}

func applyConfigDefaults(config *Config) {
	if config.CheckInterval <= 0 {
		config.CheckInterval = 5
	}
	if config.IPCheckInterval <= 0 {
		config.IPCheckInterval = 30
	}
	if config.ForceUpdateInterval <= 0 {
		config.ForceUpdateInterval = 5
	}
	if config.Timeout <= 0 {
		config.Timeout = 10
	}
	if len(config.ProbeTargets) == 0 {
		config.ProbeTargets = []string{"1.1.1.1:443", "dns.alidns.com:443"}
	}
	if config.TTL <= 0 {
		config.TTL = 1
	}
}

func saveDefaultConfig(filePath string) error {
	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "    ")
	return encoder.Encode(Config{
		AccessKey:           "",
		AccessSecret:        "",
		DomainName:          "your_domain_name",
		LogPath:             "DDns.log",
		APIURLs:             defaultAPIURLs,
		RecordType:          "A",
		RR:                  "*",
		CheckInterval:       5,
		IPCheckInterval:     30,
		ForceUpdateInterval: 5,
		ProbeTargets:        []string{"1.1.1.1:443", "dns.alidns.com:443"},
		Timeout:             10,
	})
}
