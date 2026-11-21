package cdek

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Учебная среда — общая учётка с сайта СДЭК, заказы не доставляются.
// Боевая создаёт настоящие отправления. Служебный ключ api_url (его нет
// в форме) подменяет адрес: так тест подставляет подделку и не ходит в СДЭК.
const (
	testAPI = "https://api.edu.cdek.ru"
	liveAPI = "https://api.cdek.ru"
)

var httpClient = &http.Client{
	// Срок задаёт контекст ядра (5 с расчёт, 15 с событие заказа).
	CheckRedirect: sameHost,
}

func sameHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 2 {
		return errors.New("слишком много переходов")
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return errors.New("переход на другой адрес")
	}
	return nil
}

func baseURL(s map[string]string) string {
	if u := strings.TrimRight(strings.TrimSpace(s["api_url"]), "/"); u != "" {
		return u
	}
	if s["test"] == "боевая" {
		return liveAPI
	}
	return testAPI
}

type tokenSlot struct {
	base, account, token string
	until                time.Time
}

var (
	tokMu sync.Mutex
	slot  tokenSlot
)

// token — OAuth client_credentials. Кэш на срок жизни минус минута:
// параллельные расчёты корзины не берут новый токен каждый.
func token(ctx context.Context, s map[string]string) (string, error) {
	base := baseURL(s)
	account := strings.TrimSpace(s["account"])
	tokMu.Lock()
	if slot.base == base && slot.account == account && slot.token != "" && time.Now().Before(slot.until) {
		t := slot.token
		tokMu.Unlock()
		return t, nil
	}
	tokMu.Unlock()

	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {account}, "client_secret": {s["secure"]}}
	code, raw, err := post(ctx, http.MethodPost, base+"/v2/oauth/token", "", []byte(form.Encode()), "application/x-www-form-urlencoded", true)
	if err != nil {
		return "", errors.New("СДЭК не ответил — попробуйте ещё раз")
	}
	v, _ := decode(raw).(map[string]any)
	t, _ := v["access_token"].(string)
	if code >= 400 || t == "" {
		return "", errors.New("СДЭК не принял идентификатор или пароль")
	}
	// Нет срока или он крошечный — кеш на 5 минут, не на час: протухший
	// токен иначе ходил бы с 401 до каждого запроса. Сверху — час.
	sec := 300.0
	if n, ok := asFloat(v["expires_in"]); ok && n > 120 {
		sec = n
	}
	if sec > 3600 {
		sec = 3600
	}
	tokMu.Lock()
	slot = tokenSlot{base: base, account: account, token: t, until: time.Now().Add(time.Duration(sec-60) * time.Second)}
	tokMu.Unlock()
	return t, nil
}

func dropToken() {
	tokMu.Lock()
	slot.token = ""
	tokMu.Unlock()
}

// call — запрос к API с токеном. 401 — токен протух, берём новый и
// повторяем один раз. Сеть и 502/503/504 — один повтор, кроме создания
// заказа: его повтор мог бы завести второе отправление, а повтор события
// сначала ищет заказ по номеру магазина.
func call(ctx context.Context, s map[string]string, method, path string, body any, retry bool) (int, any, error) {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
	}
	tok, err := token(ctx, s)
	if err != nil {
		return 0, nil, err
	}
	code, raw, err := post(ctx, method, baseURL(s)+path, tok, data, "application/json", retry)
	if code == http.StatusUnauthorized {
		dropToken()
		tok, err = token(ctx, s)
		if err != nil {
			return 0, nil, err
		}
		code, raw, err = post(ctx, method, baseURL(s)+path, tok, data, "application/json", false)
	}
	if err != nil {
		return code, nil, errors.New("СДЭК не ответил — попробуйте ещё раз")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if code >= 400 {
			return code, nil, errors.New("СДЭК ответил кодом " + strconv.Itoa(code))
		}
		return code, nil, nil
	}
	return code, decode(raw), nil
}

func post(ctx context.Context, method, endpoint, bearer string, data []byte, content string, retry bool) (int, []byte, error) {
	code, raw, err := postOnce(ctx, method, endpoint, bearer, data, content)
	if retry && (err != nil || code == http.StatusBadGateway || code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout) {
		code, raw, err = postOnce(ctx, method, endpoint, bearer, data, content)
	}
	return code, raw, err
}

func postOnce(ctx context.Context, method, endpoint, bearer string, data []byte, content string) (int, []byte, error) {
	var rdr io.Reader
	if data != nil {
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return 0, nil, err
	}
	if data != nil {
		req.Header.Set("Content-Type", content)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return res.StatusCode, nil, err
	}
	return res.StatusCode, raw, nil
}

func decode(raw []byte) any {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil
	}
	return v
}

func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func asInt(v any) (int, bool) {
	f, ok := asFloat(v)
	if !ok {
		return 0, false
	}
	return int(f), true
}

// cdekMessage — текст отказа без учётных данных: ошибки лежат то в
// корне, то в requests[].errors.
func cdekMessage(v any) string {
	var parts []string
	walkErr(v, &parts)
	if len(parts) == 0 {
		return ""
	}
	s := strings.Join(parts, "; ")
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

func walkErr(v any, parts *[]string) {
	switch x := v.(type) {
	case map[string]any:
		if msg, _ := x["message"].(string); strings.TrimSpace(msg) != "" && x["code"] != nil {
			*parts = append(*parts, strings.TrimSpace(msg))
		}
		for _, k := range []string{"errors", "requests"} {
			if x[k] != nil {
				walkErr(x[k], parts)
			}
		}
	case []any:
		for _, e := range x {
			walkErr(e, parts)
		}
	}
}

func obj(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func list(v any) []any {
	a, _ := v.([]any)
	return a
}
