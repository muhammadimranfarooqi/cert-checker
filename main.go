package main

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/vkuznet/x509proxy"
	"gopkg.in/jcmturner/gokrb5.v8/keytab"
)

// version of the code
var version string

// helper function to return version string of the server
func info() string {
	goVersion := runtime.Version()
	tstamp := time.Now().Format("2006-02-01")
	return fmt.Sprintf("cert-checker git=%s go=%s date=%s", version, goVersion, tstamp)
}

// main function
func main() {
	var keytabFile string
	flag.StringVar(&keytabFile, "keytab", "", "keytabFile file to check")
	var cert string
	flag.StringVar(&cert, "cert", "", "file certificate (PEM file name) or X509 proxy")
	var ckey string
	flag.StringVar(&ckey, "ckey", "", "file certficate private key (PEM file name)")
	var alert string
	flag.StringVar(&alert, "alert", "", "alert email or URL")
	var interval int
	flag.IntVar(&interval, "interval", 600, "interval before expiration (in seconds)")
	var version bool
	flag.BoolVar(&version, "version", false, "print version information about the server")
	var verbose bool
	flag.BoolVar(&verbose, "verbose", false, "print verbose information")
	var daemonInterval int
	flag.IntVar(&daemonInterval, "daemon", 0, "run as daemon with provided interval value")
	var token string
	flag.StringVar(&token, "token", "", "token string or file containing the token")
	var httpPort int
	flag.IntVar(&httpPort, "httpPort", 0, "start http server with provided http port")
	var httpBase string
	flag.StringVar(&httpBase, "httpBase", "", "http base path")
	var configFile string
	flag.StringVar(&configFile, "config", "", "read inputs from json config file")
	var teamName string
	flag.StringVar(&teamName, "team", "", "read inputs from json config file")
	flag.Parse()
	if version {
		fmt.Println(info())
		os.Exit(0)
	}
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	token = getToken(token)
	if configFile != "" {
		err := ParseConfig(configFile)
		if err != nil {
			log.Fatal(err)
		}
		path := fmt.Sprintf("%s/metrics", httpBase)
		http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			var out string
			for _, c := range Configs {
				out += checkAndGetPromMetrics(c.Cert, c.Ckey, c.Keytab, teamName, interval, verbose)
			}
			w.Write([]byte(out))
		})
		http.ListenAndServe(fmt.Sprintf(":%d", httpPort), nil)
	} else if daemonInterval > 0 {
		for {
			check(cert, ckey, keytabFile, alert, interval, token, verbose)
			time.Sleep(time.Duration(daemonInterval) * time.Second)
		}
	} else if httpPort > 0 {
		path := fmt.Sprintf("%s/metrics", httpBase)
		http.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if keytabFile != "" {
				ts, _, err := keytabExpire(keytabFile, interval, verbose)
				if err != nil {
					log.Println("unable to get keytabFile info", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				out := fmt.Sprintf("# HELP keytab_valid_sec\n")
				out += fmt.Sprintf("# TYPE keytab_valid_sec gauge\n")
				out += fmt.Sprintf("keytab_valid_sec %v\n", ts.Sub(time.Now()).Seconds())
				w.Write([]byte(out))
				return
			}
			certs, err := getCert(cert, ckey)
			if err != nil {
				log.Println("unable to get certificate info", err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			tsCert, _ := CertExpire(certs)
			out := fmt.Sprintf("# HELP cert_valid_sec\n")
			out += fmt.Sprintf("# TYPE cert_valid_sec gauge\n")
			out += fmt.Sprintf("cert_valid_sec %v\n", tsCert.Sub(time.Now()).Seconds())
			w.Write([]byte(out))
		})
		http.ListenAndServe(fmt.Sprintf(":%d", httpPort), nil)
	} else {
		check(cert, ckey, keytabFile, alert, interval, token, verbose)
	}
}

// // helper function to check and return promql metric entries
func checkAndGetPromMetrics(cert, ckey, keytab, team string, interval int, verbose bool) string {
	if keytab != "" {
		fileName := keytab[strings.LastIndex(keytab, "/")+1:]
		ts, principle, err := keytabExpire(keytab, interval, verbose)
		if err != nil {
			log.Println("unable to get keytab info", err)
			return ""
		}
		out := fmt.Sprintf("# HELP keytab_valid_sec\n")
		out += fmt.Sprintf("# TYPE keytab_valid_sec gauge\n")
		out += fmt.Sprintf(
			"keytab_valid_sec{file_name=\"%s\", principle=\"%s\", team=\"%s\"} %v\n",
			fileName, principle, team, ts.Sub(time.Now()).Seconds(),
		)
		return out
	} else if cert != "" && ckey != "" {
		fileName := cert[strings.LastIndex(cert, "/")+1:]
		certs, err := getCert(cert, ckey)
		if err != nil {
			log.Println("unable to get certificate info", err)
			return ""
		}
		tsCert, certCommonName := CertExpire(certs)
		out := fmt.Sprintf("# HELP cert_valid_sec\n")
		out += fmt.Sprintf("# TYPE cert_valid_sec gauge\n")
		out += fmt.Sprintf("cert_valid_sec{file_name=\"%s\", common_name=\"%s\" team=\"%s\"} %v\n",
			fileName, certCommonName, team, tsCert.Sub(time.Now()).Seconds())
		return out
	} else {
		return ""
	}
}

// helper function to get token
func getToken(t string) string {
	if _, err := os.Stat(t); err == nil {
		b, e := os.ReadFile(t)
		if e != nil {
			log.Fatalf("Unable to read data from file: %s, error: %s", t, e)
		}
		return strings.Replace(string(b), "\n", "", -1)
	}
	return t
}

// check given cert/key or X509 proxy for its expiration in time+interval range
func check(cert, ckey, keytab, alert string, interval int, token string, verbose bool) {
	if keytab != "" {
		ts, _, err := keytabExpire(keytab, interval, verbose)
		if ts.Sub(time.Now()).Seconds() < 0 || err != nil {
			msg := fmt.Sprintf("Keytab file '%s' has expired on %v", keytab, ts)
			if err != nil {
				msg = fmt.Sprintf("Unable to check keytab file '%s', error %v", keytab, err)
			}
			if strings.Contains(alert, "@") {
				sendEmail(alert, msg)
			} else {
				sendNotification(alert, msg, token)
			}
		}
		return
	}
	certs, err := getCert(cert, ckey)
	if err != nil {
		if strings.Contains(err.Error(), "expired") {
			if strings.Contains(alert, "@") {
				sendEmail(alert, err.Error())
			} else {
				sendNotification(alert, err.Error(), token)
			}
			return
		}
		log.Fatalf("Unable to read certificate cert=%s, ckey=%s, error=%v", cert, ckey, err)
	}
	tsCert, _ := CertExpire(certs)
	ts := time.Now().Add(time.Duration(interval) * time.Second)
	if tsCert.Before(ts) {
		msg := fmt.Sprintf("certificate timestamp: %v will expire soon", tsCert)
		if strings.Contains(alert, "@") {
			sendEmail(alert, msg)
		} else {
			sendNotification(alert, msg, token)
		}
		log.Printf("WARNING: %s alert send to %s", msg, alert)
	} else {
		log.Printf("Certificate %s expires on %v, well after interval=%d (sec) or %v", cert, tsCert, interval, ts)
	}

}

// helper function to get certificates for provide cert/key PEM files
func getCert(cert, ckey string) ([]tls.Certificate, error) {
	var x509cert tls.Certificate
	var err error
	if cert != "" && ckey != "" {
		x509cert, err = tls.LoadX509KeyPair(cert, ckey)
		if err != nil {
			return nil, fmt.Errorf("failed to parse cert/key PEM pair: %v", err)
		}
	} else {
		x509cert, err = x509proxy.LoadX509Proxy(cert)
		if err != nil {
			return nil, fmt.Errorf("failed to parse X509 proxy: %v", err)
		}
	}
	certs := []tls.Certificate{x509cert}
	return certs, nil
}

// CertExpire gets minimum certificate expire from list of certificates
func CertExpire(certs []tls.Certificate) (time.Time, string) {
	var notAfter time.Time
	var certCommonName string
	for _, cert := range certs {
		c, e := x509.ParseCertificate(cert.Certificate[0])
		certCommonName = strings.Replace(c.Subject.CommonName, "\n", "", -1)
		if e == nil {
			notAfter = c.NotAfter
			break
		}
	}
	return notAfter, certCommonName
}

// helper function to send email
func sendEmail(to, body string) {
	toList := []string{to}
	if strings.Contains(to, ",") {
		toList = strings.Split(to, ",")
	}
	host := "smtp.gmail.com"
	port := "587"
	from := os.Getenv("MAIL")
	password := os.Getenv("PASSWD")
	auth := smtp.PlainAuth("", from, password, host)
	err := smtp.SendMail(host+":"+port, auth, from, toList, []byte(body))
	if err != nil {
		log.Fatal(err)
	}
}

// helper function to send notification
func sendNotification(apiURL, msg, token string) {
	if apiURL == "" {
		log.Fatal("Unable to POST request to empty URL, please provide valid URL for alert option")
	}
	var headers [][]string
	bearer := fmt.Sprintf("Bearer %s", token)
	headers = append(headers, []string{"Authorization", bearer})
	headers = append(headers, []string{"Content-Type", "application/json"})
	req, err := http.NewRequest("POST", apiURL, bytes.NewBuffer([]byte(msg)))
	if err != nil {
		log.Fatal(err)
	}
	for _, v := range headers {
		if len(v) == 2 {
			req.Header.Set(v[0], v[1])
		}
	}
	timeout := time.Duration(1) * time.Second
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		log.Printf("Unable to send notication to %s", apiURL)
		return
	}
}

// helper function to read keytab file and return its timestamp
func keytabExpire(krbFile string, interval int, verbose bool) (time.Time, string, error) {
	ktab, err := keytab.Load(krbFile)
	if err != nil {
		return time.Now(), "", err
	}
	var ets time.Time
	var principle string
	for _, e := range ktab.Entries {
		principle = strings.Join(e.Principal.Components, ",")
		ts := e.Timestamp
		// we have 1 year accounts and would like to check if
		// keytab is expired in a future, so we do
		// keytab timestamp + 1 year - interval in seconds
		yearSecs := 365 * 24 * 60 * 60
		ets = ts.Add(time.Duration(yearSecs-interval) * time.Second)
		// secSinceCreation := ts.Sub(time.Now()).Seconds()
		if verbose {
			log.Println("### keytab entry", ts, "expire", ets, "principle", principle)
		}
		if ets.Sub(time.Now()).Seconds() < 0 {
			msg := fmt.Sprintf("keytab %s has expired, it was created on %v", krbFile, ts)
			return ts, principle, errors.New(msg)
		}
	}
	return ets, principle, nil
}

// helper function to check keytab expiration
func keytabExpireCommand(keytab string, interval int, verbose bool) (time.Time, error) {
	out, err := exec.Command("klist", "-t", "-k", keytab).Output()
	if err != nil {
		return time.Now(), err
	}
	/* here is example of output of klist command
	   klist -t -k agg.keytab
	   Keytab name: FILE:agg.keytab
	   KVNO Timestamp           Principal
	   ---- ------------------- ------------------------------------------------------
	      1 11/16/2022 14:34:08 xxx@CERN.CH
	      1 11/16/2022 14:34:08 xxx@CERN.CH
	*/
	for _, v := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(v, "Keytab") || strings.HasPrefix(v, "KVNO") || strings.HasPrefix(v, "-") {
			continue
		}
		v = strings.Trim(v, " ")
		arr := strings.Split(v, " ")
		// we need 2nd and 3rd fields to construct timestamp
		ts := strings.Trim(strings.Join(arr[1:3], " "), " ")
		const layout = "01/02/2006 03:04:05"
		t, err := time.Parse(layout, ts)
		return t, err
	}
	return time.Now(), nil
}
