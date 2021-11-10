package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/go-playground/validator/v10"
	"github.com/gorilla/mux"
	"github.com/patrickmn/go-cache"
	"github.com/prometheus/alertmanager/template"
	"golang.org/x/net/context"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

const readRange = "A2:D"

var regexpPhone = regexp.MustCompile("^\\+[1-9]\\d{1,14}$")
var regexpTwilioSid = regexp.MustCompile("^[A-Z]{2}[0-9a-f]{32}$")
var regexpSheetId = regexp.MustCompile("^[a-zA-Z0-9-_]+$")
var regexpPort = regexp.MustCompile("^([0-9]{1,4}|[1-5][0-9]{4}|6[0-4][0-9]{3}|65[0-4][0-9]{2}|655[0-2][0-9]|6553[0-5])$")
var useSentry = false

type Config struct {
	TwilioAccountSid string `validate:"required,twiliosid"`
	TwilioAuthSid    string `validate:"required,twiliosid"`
	TwilioAuthToken  string `validate:"required,min=1"`
	TwilioFromNumber string `validate:"required,phone"`
	GoogleSheetId    string `validate:"required,sheetid"`
	GoogleTokenPath  string `validate:"required,file"`
	ListenPort       string `validate:"omitempty,port"`
	SentryDsn        string `validate:"omitempty,min=1"`
}

type Server struct {
	mux http.Handler

	twilio TwilioCredentials
	google GoogleCredentials

	shortCache *cache.Cache
	longCache  *cache.Cache
}

type TwilioCredentials struct {
	AccountSid string
	AuthSid    string
	AuthToken  string
	FromNumber string
}

type GoogleCredentials struct {
	SpreadsheetId string
	TokenPath     string
}

func logMessage(message string) {
	log.Println(message)
	if useSentry {
		sentry.CaptureMessage(message)
	}
}

func asJson(w http.ResponseWriter, statusCode int, message interface{}) {
	js, err := json.Marshal(message)
	if err != nil {
		logMessage(err.Error())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	w.Write(js)
}

func newServer(config Config) *Server {
	serv := &Server{
		twilio: TwilioCredentials{config.TwilioAccountSid, config.TwilioAuthSid, config.TwilioAuthToken, config.TwilioFromNumber},
		google: GoogleCredentials{config.GoogleSheetId, config.GoogleTokenPath},
	}

	// Init router and routes
	router := mux.NewRouter()
	router.HandleFunc("/webhook", serv.webhook)
	serv.mux = router

	serv.shortCache = cache.New(10*time.Minute, 10*time.Minute)
	serv.longCache = cache.New(cache.NoExpiration, 0)

	return serv
}

func (serv *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serv.mux.ServeHTTP(w, r)
}

func (serv *Server) webhook(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	if r.Method != http.MethodPost {
		asJson(w, http.StatusMethodNotAllowed, "unsupported HTTP method")
		return
	}

	var alerts template.Data
	err := json.NewDecoder(r.Body).Decode(&alerts)
	if err != nil {
		logMessage(fmt.Sprintf("Error parsing alerts content: %s", err.Error()))
		asJson(w, http.StatusBadRequest, err.Error())
		return
	}

	for _, alert := range alerts.Alerts {
		team := alert.Labels["team"]
		message := fmt.Sprintf("%s: %s", alert.Status, alert.Annotations["summary"])
		recipients, err := getPhonesFromLabel(alert.Labels["phone_numbers"])
		if err != nil {
			logMessage(fmt.Sprintf("Cannot use label-provided phone numbers %s: %s", alert.Labels["phone_numbers"], err.Error()))
		}

		if recipients == nil {
			recipients, err = serv.getTeamNumbers(team)
			if err != nil {
				logMessage(err.Error())
				asJson(w, http.StatusInternalServerError, err.Error())
				return
			}
		}

		for _, recipient := range recipients {
			err := sendSms(serv.twilio, fmt.Sprintf("+%v", recipient), message)
			if err != nil {
				logMessage(err.Error())
				asJson(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}
	asJson(w, http.StatusOK, "success")
}

func getPhonesFromLabel(phoneNumbers string) ([]interface{}, error) {
	if phoneNumbers == "" {
		return nil, nil
	}

	phonesPattern := "^[1-9]\\d{1,14}(,[1-9]\\d{1,14})*$"
	res, err := regexp.MatchString(phonesPattern, phoneNumbers)
	if err != nil {
		return nil, err
	}
	if !res {
		return nil, errors.New("Wrong comma-separated phone numbers syntax")
	}

	split := strings.Split(phoneNumbers, ",")
	phonesList := make([]interface{}, len(split))
	for i, v := range split {
		phonesList[i] = v
	}
	return phonesList, nil
}

// Get team on-call phone number present on google sheet, use fallback cache if googleapi down
func (serv *Server) getTeamNumbers(team string) ([]interface{}, error) {
	phoneNumbers, found := serv.shortCache.Get(team)
	if found {
		return phoneNumbers.([]interface{}), nil
	}

	log.Printf("Getting numbers for team \"%s\" from Sheet", team)
	sheets, err := NewSpreadsheetService(serv.google.TokenPath)
	if err != nil {
		logMessage(fmt.Sprintf("Cannot create Sheets service, reading from fallback cache - %s", err.Error()))
		phoneNumbers, found := serv.longCache.Get(team)
		if found {
			return phoneNumbers.([]interface{}), nil
		} else {
			return nil, errors.New(fmt.Sprintf("No numbers found in fallback cache for team %s", team))
		}
	}

	resp, err := sheets.Spreadsheets.Values.Get(serv.google.SpreadsheetId, readRange).Do()
	if err != nil {
		logMessage(fmt.Sprintf("Cannot read Sheet, reading from fallback cache - %s", err.Error()))
		phoneNumbers, found := serv.longCache.Get(team)
		if found {
			return phoneNumbers.([]interface{}), nil
		} else {
			return nil, errors.New(fmt.Sprintf("No numbers found in fallback cache for team %s", team))
		}
		return nil, err
	}

	if len(resp.Values) == 0 {
		return nil, errors.New("Sheet appears to be empty :(")
	}

	for _, row := range resp.Values {
		if len(row) > 0 {
			serv.longCache.Set(row[0].(string), row[1:], cache.DefaultExpiration)
			serv.shortCache.Set(row[0].(string), row[1:], cache.DefaultExpiration)
			if row[0] == team {
				return row[1:], nil
			}
		}
	}

	return nil, errors.New(fmt.Sprintf("No row found in Sheet for team %s", team))
}

func NewSpreadsheetService(client_secret_path string) (*sheets.Service, error) {
	ctx := context.Background()
	srv, err := sheets.NewService(ctx, option.WithCredentialsFile(client_secret_path), option.WithScopes(sheets.SpreadsheetsScope))
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Unable to establish Sheets Client: %s", err.Error()))
	}
	return srv, nil
}

// Send message to recipient through twilio API
func sendSms(twilio TwilioCredentials, recipient string, message string) error {
	log.Printf("Sending SMS to %s: %s", recipient, message)

	urlStr := fmt.Sprintf("https://api.twilio.com/2010-04-01/Accounts/%s/Messages.json", twilio.AccountSid)
	msgData := url.Values{}
	msgData.Set("To", recipient)
	msgData.Set("From", twilio.FromNumber)
	msgData.Set("Body", message)
	msgDataReader := *strings.NewReader(msgData.Encode())

	client := &http.Client{}
	req, _ := http.NewRequest("POST", urlStr, &msgDataReader)
	req.SetBasicAuth(twilio.AuthSid, twilio.AuthToken)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)

	if err != nil {
		log.Printf("Error querying twilio API: %s", err.Error())
		return err
	} else if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := ioutil.ReadAll(resp.Body)
		return errors.New(fmt.Sprintf("Non-200 response from twilio API: %s - %s", resp.Status, body))
	}

	var data map[string]interface{}
	err = json.NewDecoder(resp.Body).Decode(&data)
	if err != nil {
		log.Printf("Error in twilio response body: %s", err.Error())
		return err
	}
	log.Printf("Successfully sent SMS - SID %s", data["sid"])
	return nil
}

func main() {
	validate := validator.New()
	_ = validate.RegisterValidation("phone", func(fl validator.FieldLevel) bool {
		return regexpPhone.MatchString(fl.Field().String())
	})
	_ = validate.RegisterValidation("twiliosid", func(fl validator.FieldLevel) bool {
		return regexpTwilioSid.MatchString(fl.Field().String())
	})
	_ = validate.RegisterValidation("sheetid", func(fl validator.FieldLevel) bool {
		return regexpSheetId.MatchString(fl.Field().String())
	})
	_ = validate.RegisterValidation("port", func(fl validator.FieldLevel) bool {
		return regexpPort.MatchString(fl.Field().String())
	})

	config := Config{
		TwilioAccountSid: os.Getenv("TWILIO_ACCOUNT_SID"),
		TwilioAuthSid:    os.Getenv("TWILIO_AUTH_SID"),
		TwilioAuthToken:  os.Getenv("TWILIO_AUTH_TOKEN"),
		TwilioFromNumber: os.Getenv("TWILIO_FROM_NUMBER"),
		GoogleSheetId:    os.Getenv("GOOGLE_SHEET_ID"),
		GoogleTokenPath:  os.Getenv("GOOGLE_TOKEN_PATH"),
		ListenPort:       os.Getenv("PORT"),
		SentryDsn:        os.Getenv("SENTRY_DSN"),
	}

	err := validate.Struct(config)
	if err != nil {
		for _, e := range err.(validator.ValidationErrors) {
			log.Println(e)
		}
		log.Fatal("Parameters validation failed")
	}

	if config.SentryDsn != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn: config.SentryDsn,
		})
		if err != nil {
			log.Fatal(fmt.Sprintf("Sentry initialization failed DSN %s", config.SentryDsn))
		}
		log.Printf("Sentry initialized with DSN %s", config.SentryDsn)
		defer sentry.Flush(time.Second * 5)
		defer sentry.Recover()
		useSentry = true
	} else {
		log.Println("Not using Sentry")
	}

	serv := newServer(config)

	listenAddress := ":9080"
	if config.ListenPort != "" {
		listenAddress = fmt.Sprintf(":%s", config.ListenPort)
	}

	log.Printf("listening on: %s", listenAddress)

	log.Fatal(http.ListenAndServe(listenAddress, serv))
}
