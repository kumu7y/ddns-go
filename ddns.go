package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aliyun/alibaba-cloud-sdk-go/services/alidns"
)

type Config struct {
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
}

var ErrNoUpdateNeeded = errors.New("DNS record unchanged, no update needed")

func getPublicIPWithFallback(apiURLs []string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	var lastErr error

	for _, url := range apiURLs {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			continue
		}

		body, err := io.ReadAll(resp.Body)
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
		if err := json.Unmarshal(body, &result); err == nil {
			if ip, ok := result["ip"].(string); ok && ip != "" {
				return ip, nil
			}
			if origin, ok := result["origin"].(string); ok && origin != "" {
				return origin, nil
			}
		}

		if strings.Count(text, ".") >= 3 && !strings.Contains(text, " ") && !strings.Contains(text, "<") {
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

	_, err = client.AddDomainRecord(addRequest)
	return err
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
		if err := os.MkdirAll(logDir, 0755); err != nil {
			log.Fatal("Failed to create log directory:", err)
		}
	}

	logFile, err := os.OpenFile(logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}
	defer logFile.Close()

	multiWriter := io.MultiWriter(os.Stdout, logFile)
	logger := log.New(multiWriter, "DDns: ", log.LstdFlags|log.Lmicroseconds)

	client, err := alidns.NewClientWithAccessKey("cn-hangzhou", config.AccessKey, config.AccessSecret)
	if err != nil {
		logger.Fatalf("Failed to create Aliyun DNS client: %v", err)
	}

	apiURLs := config.APIURLs
	if len(apiURLs) == 0 {
		apiURLs = []string{
			"https://checkip.amazonaws.com",
			"https://ipv4.icanhazip.com",
			"https://ip.3322.net",
			"https://ipinfo.io/json",
			"https://api.ipify.org?format=json",
		}
	}

	domainName := config.DomainName
	httpTimeout := time.Duration(config.Timeout) * time.Second
	probeTimeout := 3 * time.Second

	lastKnownIP := ""
	wasReachable := true

	if checkReachability(config.ProbeTargets, probeTimeout) {
		ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
		if ip != "" {
			lastKnownIP = ip
			logger.Printf("Initial public IP: %s, updating DNS records...", ip)
			lastKnownIP = updateAllRRs(logger, client, config, ip)
		}
	} else {
		logger.Println("Network unreachable at startup, waiting for recovery...")
	}

	probeTicker := time.NewTicker(time.Duration(config.CheckInterval) * time.Second)
	ipCheckTicker := time.NewTicker(time.Duration(config.IPCheckInterval) * time.Second)
	forceTicker := time.NewTicker(time.Duration(config.ForceUpdateInterval) * time.Minute)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	defer func() {
		probeTicker.Stop()
		ipCheckTicker.Stop()
		forceTicker.Stop()
	}()

	logger.Printf("DDNS started, domain: %s, rr: %s, checkInterval: %ds, ipCheckInterval: %ds, forceUpdateInterval: %dm",
		domainName, config.RR, config.CheckInterval, config.IPCheckInterval, config.ForceUpdateInterval)

	for {
		select {
		case <-probeTicker.C:
			reachable := checkReachability(config.ProbeTargets, probeTimeout)
			if !wasReachable && reachable {
				logger.Println("Network recovered, checking IP immediately...")
				ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
				if ip != "" && ip != lastKnownIP {
					lastKnownIP = updateAllRRs(logger, client, config, ip)
				} else if ip != "" {
					logger.Printf("IP unchanged: %s", ip)
				}
			}
			wasReachable = reachable

		case <-ipCheckTicker.C:
			ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
			if ip != "" && ip != lastKnownIP {
				logger.Printf("IP changed: %s -> %s", lastKnownIP, ip)
				lastKnownIP = updateAllRRs(logger, client, config, ip)
			}

		case <-forceTicker.C:
			ip := fetchAndLogIP(logger, apiURLs, httpTimeout)
			if ip != "" {
				if ip != lastKnownIP {
					logger.Printf("Force update: IP changed from %s to %s", lastKnownIP, ip)
				}
				lastKnownIP = updateAllRRs(logger, client, config, ip)
			}

		case sig := <-sigChan:
			logger.Printf("Received signal %v, shutting down...", sig)
			return
		}
	}
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
	for _, r := range rrs {
		currentRR := strings.TrimSpace(r)
		if currentRR == "" {
			continue
		}
		err := updateDNSRecord(client, config.DomainName, publicIP, config.RecordType, currentRR)
		if err != nil {
			if err == ErrNoUpdateNeeded {
				logger.Printf("RR '%s' already points to %s, skipped", currentRR, publicIP)
			} else {
				logger.Printf("Failed to update RR '%s': %v", currentRR, err)
				success = false
			}
		} else {
			logger.Printf("RR '%s' updated successfully to %s", currentRR, publicIP)
		}
	}
	if success {
		return publicIP
	}
	return ""
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
		APIURLs: []string{
			"https://checkip.amazonaws.com",
			"https://ipv4.icanhazip.com",
			"https://ip.3322.net",
			"https://ipinfo.io/json",
			"https://api.ipify.org?format=json",
		},
		RecordType:          "A",
		RR:                  "*",
		CheckInterval:       5,
		IPCheckInterval:     30,
		ForceUpdateInterval: 5,
		ProbeTargets:        []string{"1.1.1.1:443", "dns.alidns.com:443"},
		Timeout:             10,
	})
}
