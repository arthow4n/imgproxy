package main

import (
	"compress/gzip"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	nanoid "github.com/matoous/go-nanoid"
)

var mimes = map[imageType]string{
	JPEG: "image/jpeg",
	PNG:  "image/png",
	WEBP: "image/webp",
}

type httpHandler struct {
	sem chan struct{}
}

func newHTTPHandler() *httpHandler {
	return &httpHandler{make(chan struct{}, conf.Concurrency)}
}

func parsePath(r *http.Request) (string, processingOptions, error) {
	var po processingOptions
	var err error

	path := r.URL.Path
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")

	if len(parts) < 7 {
		return "", po, errors.New("Invalid path")
	}

	token := parts[0]

	if err = validatePath(token, strings.TrimPrefix(path, fmt.Sprintf("/%s", token))); err != nil {
		return "", po, err
	}

	if r, ok := resizeTypes[parts[1]]; ok {
		po.Resize = r
	} else {
		return "", po, fmt.Errorf("Invalid resize type: %s", parts[1])
	}

	if po.Width, err = strconv.Atoi(parts[2]); err != nil {
		return "", po, fmt.Errorf("Invalid width: %s", parts[2])
	}

	if po.Height, err = strconv.Atoi(parts[3]); err != nil {
		return "", po, fmt.Errorf("Invalid height: %s", parts[3])
	}

	if g, ok := gravityTypes[parts[4]]; ok {
		po.Gravity = g
	} else {
		return "", po, fmt.Errorf("Invalid gravity: %s", parts[4])
	}

	po.Enlarge = parts[5] != "0"

	filenameParts := strings.Split(strings.Join(parts[6:], ""), ".")

	if len(filenameParts) < 2 {
		po.Format = imageTypes["jpg"]
	} else if f, ok := imageTypes[filenameParts[1]]; ok {
		po.Format = f
	} else {
		return "", po, fmt.Errorf("Invalid image format: %s", filenameParts[1])
	}

	if !vipsTypeSupportSave[po.Format] {
		return "", po, errors.New("Resulting image type not supported")
	}

	filename, err := base64.RawURLEncoding.DecodeString(filenameParts[0])
	if err != nil {
		return "", po, errors.New("Invalid filename encoding")
	}

	return string(filename), po, nil
}

func logResponse(status int, msg string) {
	var color int

	if status >= 500 {
		color = 31
	} else if status >= 400 {
		color = 33
	} else {
		color = 32
	}

	log.Printf("|\033[7;%dm %d \033[0m| %s\n", color, status, msg)
}

func respondWithImage(reqID string, r *http.Request, rw http.ResponseWriter, data []byte, imgURL string, po processingOptions, duration time.Duration) {
	gzipped := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && conf.GZipCompression > 0

	rw.Header().Set("Expires", time.Now().Add(time.Second*time.Duration(conf.TTL)).Format(http.TimeFormat))
	rw.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d, public", conf.TTL))
	rw.Header().Set("Content-Type", mimes[po.Format])

	if gzipped {
		rw.Header().Set("Content-Encoding", "gzip")
	}

	rw.WriteHeader(200)

	if gzipped {
		gz, _ := gzip.NewWriterLevel(rw, conf.GZipCompression)
		gz.Write(data)
		gz.Close()
	} else {
		rw.Write(data)
	}

	logResponse(200, fmt.Sprintf("[%s] Processed in %s: %s; %+v", reqID, duration, imgURL, po))
}

func respondWithError(reqID string, rw http.ResponseWriter, err imgproxyError) {
	logResponse(err.StatusCode, fmt.Sprintf("[%s] %s", reqID, err.Message))

	rw.WriteHeader(err.StatusCode)
	rw.Write([]byte(err.PublicMessage))
}

func checkSecret(s string) bool {
	if len(conf.Secret) == 0 {
		return true
	}
	return strings.HasPrefix(s, "Bearer ") && subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(s, "Bearer ")), []byte(conf.Secret)) == 1
}

func (h *httpHandler) lock() {
	h.sem <- struct{}{}
}

func (h *httpHandler) unlock() {
	<-h.sem
}

func (h *httpHandler) ServeHTTP(rw http.ResponseWriter, r *http.Request) {
	reqID, _ := nanoid.Nanoid()

	log.Printf("[%s] GET: %s\n", reqID, r.URL.RequestURI())

	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(imgproxyError); ok {
				respondWithError(reqID, rw, err)
			} else {
				respondWithError(reqID, rw, newUnexpectedError(r.(error), 4))
			}
		}
	}()

	if !checkSecret(r.Header.Get("Authorization")) {
		panic(invalidSecretErr)
	}

	h.lock()
	defer h.unlock()

	if r.URL.Path == "/health" {
		rw.WriteHeader(200)
		rw.Write([]byte("imgproxy is running"))
		return
	}

	t := startTimer(time.Duration(conf.WriteTimeout)*time.Second, "Processing")

	imgURL, procOpt, err := parsePath(r)
	if err != nil {
		panic(newError(404, err.Error(), "Invalid image url"))
	}

	if _, err = url.ParseRequestURI(imgURL); err != nil {
		panic(newError(404, err.Error(), "Invalid image url"))
	}

	b, imgtype, err := downloadImage(imgURL)
	if err != nil {
		panic(newError(404, err.Error(), "Image is unreachable"))
	}

	t.Check()

	if conf.ETagEnabled {
		eTag := calcETag(b, &procOpt)
		rw.Header().Set("ETag", eTag)

		if eTag == r.Header.Get("If-None-Match") {
			panic(notModifiedErr)
		}
	}

	t.Check()

	b, err = processImage(b, imgtype, procOpt, t)
	if err != nil {
		panic(newError(500, err.Error(), "Error occurred while processing image"))
	}

	t.Check()

	respondWithImage(reqID, r, rw, b, imgURL, procOpt, t.Since())
}
