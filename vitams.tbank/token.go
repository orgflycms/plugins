package tbank

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

// defaultAPI — боевой адрес эквайринга. Служебный ключ настроек api_url
// подменяет его в тестах; в форме настроек его нет.
const defaultAPI = "https://securepay.tinkoff.ru/v2"

// token — подпись запроса и уведомления Т-Банка: значения всех простых
// полей корня (без Token и вложенных объектов — Receipt, DATA) и пароль
// терминала под ключом Password, по алфавиту ключей, подряд, SHA-256.
//
// Запрос и уведомление подписываются одной функцией по тому же набору
// полей, что уходит в JSON: поле, добавленное в запрос, не может выпасть из
// подписи, а новое поле уведомления банка не ломает проверку.
func token(fields map[string]any, password string) string {
	values := map[string]string{"Password": password}
	for k, v := range fields {
		if k == "Token" || k == "Password" {
			continue
		}
		if s, ok := scalar(v); ok {
			values[k] = s
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(values[k])
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// scalar — значение поля строкой, как его видит банк. Числа уведомления
// приходят json.Number (decode ниже): через float64 длинный PaymentId
// превратился бы в 1.2345e+10 и дал другую подпись. Объекты, массивы и null
// в подпись не входят.
func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return x.String(), true
	case bool:
		return strconv.FormatBool(x), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case int:
		return strconv.Itoa(x), true
	}
	return "", false
}

// decode разбирает JSON банка, не трогая числа.
func decode(data []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var out map[string]any
	if err := d.Decode(&out); err != nil {
		return nil, err
	}
	if out == nil {
		return nil, errors.New("пустой ответ")
	}
	return out, nil
}

// text — простое поле ответа строкой: PaymentId банк отдаёт то строкой, то
// числом.
func text(m map[string]any, key string) string {
	s, _ := scalar(m[key])
	return s
}

// amount — сумма в копейках из ответа или уведомления; не число — 0, и
// ядро такую оплату не отметит.
func amount(m map[string]any) int64 {
	n, err := strconv.ParseInt(text(m, "Amount"), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// call — подписанный запрос к методу API (Init, GetState). Ошибка — банк
// недоступен или отказал; текст отказа — слова банка с кодом, без ключа и
// пароля.
func call(ctx context.Context, settings map[string]string, method string, body map[string]any) (map[string]any, error) {
	body["TerminalKey"] = strings.TrimSpace(settings["terminal_key"])
	body["Token"] = token(body, settings["password"])
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(settings["api_url"]), "/")
	if base == "" {
		base = defaultAPI
	}
	res, err := postBank(ctx, base+"/"+method, data)
	if err != nil {
		return nil, errors.New("банк недоступен")
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, errors.New("банк недоступен")
	}
	out, err := decode(raw)
	if err != nil {
		return nil, errors.New("банк ответил кодом " + strconv.Itoa(res.StatusCode) + " без данных")
	}
	if ok, _ := out["Success"].(bool); !ok {
		return out, errors.New(refusal(out))
	}
	return out, nil
}

// refusal — отказ банка одной строкой: «Неверные параметры. Поле Amount…,
// код 9».
func refusal(m map[string]any) string {
	parts := []string{}
	for _, k := range []string{"Message", "Details"} {
		if s := strings.TrimSpace(text(m, k)); s != "" {
			parts = append(parts, strings.TrimRight(s, "."))
		}
	}
	s := strings.Join(parts, ". ")
	if s == "" {
		s = "отказ без объяснения"
	}
	if code := text(m, "ErrorCode"); code != "" && code != "0" {
		s += ", код " + code
	}
	return s
}
