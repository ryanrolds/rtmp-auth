package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/csrf"
	"github.com/voc/rtmp-auth/storage"
	"github.com/voc/rtmp-auth/store"
)

type handleFunc func(http.ResponseWriter, *http.Request)

var durationRegex = regexp.MustCompile(`P([\d\.]+Y)?([\d\.]+M)?([\d\.]+D)?T?([\d\.]+H)?([\d\.]+M)?([\d\.]+?S)?`)

func parseDurationPart(value string, unit time.Duration) time.Duration {
	if len(value) != 0 {
		if parsed, err := strconv.ParseFloat(value[:len(value)-1], 64); err == nil {
			return time.Duration(float64(unit) * parsed)
		}
	}
	return 0
}

// Parse expiration time
func parseExpiry(str string) *int64 {
	// Allow empty string for "never"
	if str == "" {
		never := int64(-1)
		return &never
	}

	// Try to parse as ISO8601 duration
	matches := durationRegex.FindStringSubmatch(str)
	if matches != nil {
		years := parseDurationPart(matches[1], time.Hour*24*365)
		months := parseDurationPart(matches[2], time.Hour*24*30)
		days := parseDurationPart(matches[3], time.Hour*24)
		hours := parseDurationPart(matches[4], time.Hour)
		minutes := parseDurationPart(matches[5], time.Second*60)
		seconds := parseDurationPart(matches[6], time.Second)
		d := time.Duration(years + months + days + hours + minutes + seconds)
		if d == 0 {
			return nil
		}

		expiry := time.Now().Add(d).Unix()
		return &expiry
	}

	// Try to parse as absolute time
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return nil
	}
	expiry := t.Unix()
	return &expiry
}

type SRSPublish struct {
	Action string `json:"action"`
	IP     string `json:"ip"`
	VHost  string `json:"vhost"`
	App    string `json:"app"`
	Url    string `json:"tcUrl"`
	Stream string `json:"stream"`
	Param  string `json:"param"`
}

func handleSRSRequest(r *http.Request) (app string, name string, auth string, action string, err error) {
	var publish SRSPublish

	if r.ContentLength == 0 {
		err = errors.New("empty body")
		return
	}

	body := make([]byte, r.ContentLength)
	_, err = r.Body.Read(body)
	if err != nil {
		log.Printf("Failed to read body: %s", err)
		return
	}

	log.Printf("SRS request: %s\n", body)

	err = json.Unmarshal(body, &publish)
	if err != nil {
		return
	}

	// skip question mark
	if len(publish.Param) > 0 {
		publish.Param = publish.Param[1:]
	}

	val, err := url.ParseQuery(publish.Param)
	if err != nil {
		return
	}
	app = publish.App
	name = publish.Stream
	auth = val.Get("auth")
	action = publish.Action
	return
}

func handleNginxRequest(r *http.Request) (app string, name string, auth string, action string, err error) {
	err = r.ParseForm()
	if err != nil {
		return
	}

	app = r.PostForm.Get("app")
	name = r.PostForm.Get("name")
	auth = r.PostForm.Get("auth")
	action = r.PostForm.Get("call")
	log.Printf("Nginx request: %s %s %s %s", app, name, auth, action)

	var body []byte
	if r.ContentLength != 0 {
		body, err = io.ReadAll(r.Body)
		if err != nil {
			return
		}

		log.Printf("Nginx request body: %s\n", string(body))
	} else {
		log.Println("Nginx request: empty body")
	}

	return
}

// AuthHandler checks requests for authentication
func AuthHandler(store *store.Store) handleFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var app string
		var name string
		var auth string
		var action string
		var err error
		if r.Header.Get("Content-Type") == "application/json" {
			// SRS handler
			app, name, auth, action, err = handleSRSRequest(r)
		} else {
			// Form DATA from nginx-rtmp/srtrelay
			app, name, auth, action, err = handleNginxRequest(r)
		}
		if err != nil {
			log.Println("Failed to parse play data:", err)
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}

		success, id := store.Auth(app, name, auth)
		if !success {
			log.Printf("%s %s %s/%s unauthorized\n", action, id, app, name)
			http.Error(w, "401 Unauthorized", http.StatusUnauthorized)
			return
		}

		if action == "on_publish" || action == "publish" {
			store.SetActive(id)
		} else if action == "on_unpublish" || action == "unpublish" {
			store.SetInactive(app, name)
		}

		log.Printf("%s %s %s/%s ok\n", action, id, app, name)

		// SRS needs zero response
		w.Write([]byte("0"))
	}
}

func FormHandler(store *store.Store, config ServerConfig) handleFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var errs []error
		state, err := store.Get()
		if err != nil {
			errs = append(errs, err)
		}

		sort.SliceStable(state.Streams, func(i, j int) bool {
			return state.Streams[i].Name < state.Streams[j].Name
		})

		data := TemplateData{
			State:        state,
			Config:       config,
			CsrfTemplate: csrf.TemplateField(r),
			Errors:       errs,
		}
		err = templates.ExecuteTemplate(w, "form.html", data)
		if err != nil {
			log.Println("Template failed", err)
		}
	}
}

func AddHandler(store *store.Store, config ServerConfig) handleFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var errs []error

		expiry := parseExpiry(r.PostFormValue("auth_expire"))
		if expiry == nil {
			errs = append(errs, fmt.Errorf("invalid auth expiry: '%v'", r.PostFormValue("auth_expire")))
		}

		name := r.PostFormValue("name")
		if len(name) == 0 {
			errs = append(errs, fmt.Errorf("stream name must be set"))
		}

		// TODO: more validation
		if len(errs) == 0 {
			stream := &storage.Stream{
				Name:        name,
				Application: r.PostFormValue("application"),
				AuthKey:     r.PostFormValue("auth_key"),
				AuthExpire:  *expiry,
				Notes:       r.PostFormValue("notes"),
			}

			err := store.AddStream(stream)
			if err != nil {
				errs = append(errs, fmt.Errorf("failed to add stream: %w", err))
			} else {
				http.Redirect(w, r, config.Prefix, http.StatusSeeOther)
			}
		}

		state, err := store.Get()
		if err != nil {
			errs = append(errs, err)
		}
		data := TemplateData{
			State:        state,
			Config:       config,
			CsrfTemplate: csrf.TemplateField(r),
			Errors:       errs,
		}
		err = templates.ExecuteTemplate(w, "form.html", data)
		if err != nil {
			log.Println("Template failed", err)
		}
	}
}

func RemoveHandler(store *store.Store, config ServerConfig) handleFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var errs []error
		id := r.PostFormValue("id")

		err := store.RemoveStream(id)
		if err != nil {
			log.Println(err)
			errs = append(errs, fmt.Errorf("failed to remove stream: %w", err))
			state, err := store.Get()
			if err != nil {
				errs = append(errs, err)
			}
			data := TemplateData{
				State:        state,
				Config:       config,
				CsrfTemplate: csrf.TemplateField(r),
				Errors:       errs,
			}
			err = templates.ExecuteTemplate(w, "form.html", data)
			if err != nil {
				log.Println("Template failed", err)
			}
		} else {
			http.Redirect(w, r, config.Prefix, http.StatusSeeOther)
		}
	}
}

func BlockHandler(store *store.Store, config ServerConfig) handleFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var errs []error
		id := r.PostFormValue("id")
		last, _ := strconv.ParseBool(r.PostFormValue("blocked"))
		new := false
		action := "unblock"
		if !last {
			new = true
			action = "block"
		}

		// Get Application/Name for stream id
		var app, name string
		state, err := store.Get()
		if err != nil {
			errs = append(errs, err)
		}
		for _, stream := range state.Streams {
			if stream.Id == id {
				app = stream.Application
				name = stream.Name
			}
		}

		err = store.SetBlocked(id, new)
		if err != nil {
			log.Println(err)
			errs = append(errs, fmt.Errorf("failed to %v stream %v (%v/%v)", action, id, app, name))
		}
		log.Printf("%ved Stream %v (%v/%v)", action, id, app, name)
		if len(errs) > 0 {
			data := TemplateData{
				State:        state,
				Config:       config,
				CsrfTemplate: csrf.TemplateField(r),
				Errors:       errs,
			}
			err = templates.ExecuteTemplate(w, "form.html", data)
			if err != nil {
				log.Println("Template failed", err)
			}
		} else {
			http.Redirect(w, r, config.Prefix, http.StatusSeeOther)
		}
	}
}
