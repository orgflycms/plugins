// Плагин «Счёт через Т-Банк Бизнес»: способ оплаты, при котором счёт
// выставляет банк. Покупатель-организация нажимает «Скачать счёт» — плагин
// создаёт счёт по API Т-Бизнеса и отдаёт PDF банка; оплачен ли счёт, ядро
// узнаёт опросом (Check), и владельцу не нужно сверять выписку.
//
// Уведомлений у счетов Т-Бизнеса нет, поэтому Notify нет. База плагину не
// нужна: номер счёта и адрес PDF ядро хранит в оплате (ExternalID,
// ExternalURL) и отдаёт обратно при каждом вызове.
package tbank_invoice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/orgflycms/sdk"
)

const (
	defaultAPI       = "https://business.tbank.ru/openapi/api/v1"
	defaultPDFPrefix = "https://business.tbank.ru/invoices/"
	defaultDueDays   = 5
)

// vatOptions — подписи ставок для владельца; vatCode переводит их в
// значения API счетов. Ставки — на 2026 год, как в vitams.invoice.
var vatOptions = []string{"Без НДС", "НДС 0%", "НДС 5%", "НДС 7%", "НДС 10%", "НДС 22%"}

func vatCode(label string) string {
	rate := strings.TrimSuffix(strings.TrimPrefix(label, "НДС "), "%")
	if label == "" || label == "Без НДС" || rate == label {
		return "None"
	}
	return rate
}

// fields — вкладка «Настройки» способа. Адрес API и префикс адреса PDF
// (api_url, pdf_prefix) в форму не вынесены: они нужны только тестам и
// задаются в таблице «ключ — значение» под формой.
var fields = []sdk.Field{
	{Key: "token", Label: "Токен API Т-Бизнеса", Kind: "password", Required: true,
		Hint: "Выпускается в Т-Бизнесе, раздел «Интеграции»; токену нужен доступ к выставлению счетов."},
	{Key: "account", Label: "Расчётный счёт", Required: true,
		Hint: "Счёт в Т-Банке, на который придут деньги: 20 цифр. Счёт выставляется от имени его владельца."},
	{Key: "vat", Label: "НДС", Kind: "select", Options: vatOptions,
		Hint: "Ставка для всех строк счёта. Пусто — «Без НДС»."},
	{Key: "due_days", Label: "Срок оплаты, дней", Kind: "number",
		Hint: "Сколько дней от даты заказа счёт ждёт оплаты. Пусто — 5."},
	{Key: "comment", Label: "Комментарий к счёту", Kind: "textarea",
		Hint: "Печатается в счёте: условия оплаты и поставки."},
}

// api — клиент API банка. Срок короче срока вызова ядра (Start — 20 с,
// Check — 15 с): после ответа банка ещё нужно успеть скачать PDF.
var api = &http.Client{Timeout: 8 * time.Second}

func Init(a sdk.API) error {
	if err := initPDF(); err != nil {
		return err
	}
	a.RegisterPayment("invoice", sdk.PaymentHandler{
		Title:    "Счёт через Т-Банк Бизнес",
		Button:   "Скачать счёт",
		Settings: fields,
		Start:    start,
		Check:    check,
	})
	return nil
}

// start отдаёт PDF счёта. Счёт в банке создаётся один раз: его номер ядро
// запоминает даже вместе с ошибкой, и дальше каждый вызов только скачивает
// файл — сколько бы раз покупатель ни открыл ссылку.
func start(ctx context.Context, p *sdk.Payment) (sdk.PayStart, error) {
	name := "Счёт " + strconv.FormatInt(p.OrderID, 10) + ".pdf"
	prefix := setting(p, "pdf_prefix", defaultPDFPrefix)

	if p.ExternalID != "" {
		if p.ExternalURL == "" {
			return sdk.PayStart{}, errors.New("Счёт выставлен, но банк не дал ссылку на файл — свяжитесь с магазином, пожалуйста")
		}
		data, err := fetchPDF(ctx, p.ExternalURL, prefix)
		if err != nil {
			return sdk.PayStart{}, pdfLater(err)
		}
		return sdk.PayStart{File: &sdk.File{Name: name, Data: data}}, nil
	}
	// Оплату отметил владелец, а счёт в банке не выставляли: новый счёт
	// по оплаченному заказу висел бы в банке неоплаченным.
	if p.Status == sdk.PayPaid {
		return sdk.PayStart{}, nil
	}

	// Ядро прячет способ с пустым обязательным полем от новых заказов, но
	// ссылка старого заказа остаётся в письме и кабинете.
	token, account := setting(p, "token", ""), strings.ReplaceAll(setting(p, "account", ""), " ", "")
	if token == "" || account == "" {
		return sdk.PayStart{}, errors.New("Магазин ещё не настроил выставление счетов — свяжитесь с ним, пожалуйста")
	}
	inn := strings.TrimSpace(p.Payer.INN)
	if inn == "" {
		return sdk.PayStart{}, errors.New("Для счёта нужен ИНН плательщика — оформите заказ как организация или свяжитесь с магазином")
	}

	req := invoiceRequest(p, account, inn)
	endpoint := strings.TrimRight(setting(p, "api_url", defaultAPI), "/") + "/invoice/send"
	reply, code, err := call(ctx, http.MethodPost, endpoint, token, req)
	// Телефон на счёт не влияет, а его проверка у банка время от времени
	// отклоняет счёт целиком — тогда повтор без телефона.
	if err == nil && req["contactPhone"] != nil && strings.Contains(reply.ErrorMessage+string(reply.ErrorDetails), "contactPhone") {
		delete(req, "contactPhone")
		reply, code, err = call(ctx, http.MethodPost, endpoint, token, req)
	}
	if err != nil {
		return sdk.PayStart{}, errors.New("Банк не ответил — попробуйте ещё раз через минуту (" + err.Error() + ")")
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return sdk.PayStart{}, errors.New("Банк не принял запрос магазина на счёт (код " + strconv.Itoa(code) + ") — свяжитесь с магазином, пожалуйста")
	}
	if msg := bankError(reply, code); msg != "" {
		return sdk.PayStart{}, errors.New("Банк не выставил счёт: " + msg + ". Проверьте реквизиты в заказе или свяжитесь с магазином")
	}
	if reply.InvoiceID == "" {
		return sdk.PayStart{}, errors.New("Банк не вернул номер счёта — свяжитесь с магазином, пожалуйста")
	}

	// Счёт уже в банке: что бы ни случилось дальше, его номер уходит ядру.
	res := sdk.PayStart{ExternalID: reply.InvoiceID}
	if err := checkPDFURL(reply.PdfURL, prefix); err != nil {
		return res, errors.New("Счёт выставлен, но банк не дал ссылку на файл — свяжитесь с магазином, пожалуйста")
	}
	res.URL = reply.PdfURL
	data, err := fetchPDF(ctx, reply.PdfURL, prefix)
	if err != nil {
		return res, pdfLater(err)
	}
	res.File = &sdk.File{Name: name, Data: data}
	return res, nil
}

// pdfLater — счёт есть, файла пока нет. Причина в скобках — для журнала
// магазина: туда ошибка Start попадает тем же текстом.
func pdfLater(err error) error {
	return errors.New("Счёт выставлен, файл пока недоступен — откройте ссылку через минуту (" + err.Error() + ")")
}

// check спрашивает банк о счёте. Текст ошибки — для владельца: его
// показывает кнопка «Проверить оплату» на вкладке оплат заказа.
func check(ctx context.Context, p *sdk.Payment) (sdk.PayEvent, error) {
	token := setting(p, "token", "")
	if token == "" {
		return sdk.PayEvent{}, errors.New("в настройках способа оплаты не задан токен API Т-Бизнеса")
	}
	// Путь с двумя openapi — не опечатка: так он записан у банка.
	endpoint := strings.TrimRight(setting(p, "api_url", defaultAPI), "/") + "/openapi/invoice/" + url.PathEscape(p.ExternalID) + "/info"
	reply, code, err := call(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return sdk.PayEvent{}, errors.New("Т-Банк не ответил на вопрос о счёте: " + err.Error())
	}
	if code == http.StatusUnauthorized || code == http.StatusForbidden {
		return sdk.PayEvent{}, errors.New("Т-Банк не принял токен API (код " + strconv.Itoa(code) +
			"): проверьте в настройках способа оплаты токен, его срок и доступ к счетам")
	}
	if msg := bankError(reply, code); msg != "" {
		return sdk.PayEvent{}, errors.New("Т-Банк не сообщил статус счёта " + p.ExternalID + ": " + msg)
	}
	switch reply.Status {
	case "":
		return sdk.PayEvent{}, errors.New("Т-Банк не сообщил статус счёта " + p.ExternalID + ": в ответе нет статуса")
	case "EXECUTED":
		// Суммы в ответе о статусе нет, а сумма выставленного счёта не
		// меняется: счёт создан на p.Amount, его же и подтверждаем.
		return sdk.PayEvent{Status: sdk.PayPaid, Amount: p.Amount, ExternalStatus: reply.Status}, nil
	}
	// DRAFT, SUBMITTED и всё незнакомое — ждём: слово банка владелец
	// увидит в заказе.
	return sdk.PayEvent{Status: sdk.PayPending, ExternalStatus: reply.Status}, nil
}

func setting(p *sdk.Payment, key, fallback string) string {
	if v := strings.TrimSpace(p.Settings[key]); v != "" {
		return v
	}
	return fallback
}

// invoiceRequest — тело запроса на счёт. Номер и дата счёта — от заказа,
// как у vitams.invoice: покупатель видит один номер в магазине и в банке.
func invoiceRequest(p *sdk.Payment, account, inn string) map[string]any {
	days := defaultDueDays
	if n, err := strconv.Atoi(setting(p, "due_days", "")); err == nil && n > 0 {
		days = n
	}
	// Ссылку из письма открывают и через месяц: срок оплаты в прошлом
	// банк не примет, поэтому он не раньше сегодняшнего дня.
	due := p.Created.AddDate(0, 0, days)
	if today := time.Now().In(p.Created.Location()); due.Before(today) {
		due = today
	}

	payer := map[string]any{"name": strings.TrimSpace(p.Payer.Company), "inn": inn}
	if payer["name"] == "" {
		payer["name"] = strings.TrimSpace(p.Payer.Name)
	}
	if kpp := strings.TrimSpace(p.Payer.KPP); kpp != "" {
		payer["kpp"] = kpp
	}
	req := map[string]any{
		"invoiceNumber": strconv.FormatInt(p.OrderID, 10),
		"invoiceDate":   p.Created.Format("2006-01-02"),
		"dueDate":       due.Format("2006-01-02"),
		"accountNumber": account,
		"payer":         payer,
		"items":         invoiceItems(p, vatCode(setting(p, "vat", ""))),
	}
	if email := strings.TrimSpace(p.Payer.Email); email != "" {
		req["contacts"] = []map[string]any{{"email": email}}
	}
	if phone := phoneE164(p.Payer.Phone); phone != "" {
		req["contactPhone"] = phone
	}
	if comment := setting(p, "comment", ""); comment != "" {
		req["comment"] = comment
	}
	return req
}

// invoiceItems — строки счёта. Банк считает сумму строки сам, как цена ×
// количество, поэтому цена — со скидкой (Sum / Qty). Если она не выражается
// в копейках (скидка заказа разнесена по строкам, количество дробное),
// строка идёт одной позицией на всю сумму с количеством в названии: иначе
// итог счёта разошёлся бы с суммой заказа на копейки, а ядро подтверждает
// оплату только при точном совпадении.
func invoiceItems(p *sdk.Payment, vat string) []map[string]any {
	items := []map[string]any{}
	for _, it := range p.Items {
		if it.Sum <= 0 {
			continue // подарок: строку с нулевой ценой банк не принимает, на итог она не влияет
		}
		name, unit, qty := it.Name, it.Unit, it.Qty
		if unit == "" {
			unit = "шт"
			if it.Delivery {
				unit = "усл"
			}
		}
		price := int64(0)
		if qty > 0 {
			price = int64(math.Round(float64(it.Sum) / qty))
		}
		if qty <= 0 || math.Abs(float64(price)*qty-float64(it.Sum)) > 0.001 {
			if qty > 0 && qty != 1 {
				name += ", " + strings.Replace(strconv.FormatFloat(qty, 'f', -1, 64), ".", ",", 1) + " " + unit
				unit = "шт"
			}
			qty, price = 1, it.Sum
		}
		items = append(items, map[string]any{"name": name, "unit": unit, "vat": vat, "price": rubles(price), "amount": qty})
	}
	return items
}

// rubles — копейки числом JSON в рублях: 123456 → 1234.56. Строкой, а не
// через float64: так в запросе ровно два знака и нет ошибок округления.
func rubles(kopecks int64) json.Number {
	s := strconv.FormatInt(kopecks%100, 10)
	if len(s) < 2 {
		s = "0" + s
	}
	return json.Number(strconv.FormatInt(kopecks/100, 10) + "." + s)
}

// phoneE164 — телефон из формы заказа в виде +7XXXXXXXXXX, как его ждёт
// банк; непохожий на российский номер не отправляется вовсе.
func phoneE164(phone string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	if len(digits) == 10 {
		return "+7" + digits
	}
	if len(digits) == 11 && (digits[0] == '7' || digits[0] == '8') {
		return "+7" + digits[1:]
	}
	return ""
}

// bankReply — поля ответов API, которые нужны плагину: счёт, статус и
// описание отказа. errorDetails банк шлёт то строкой, то объектом
// {поле: описание} — поэтому сырым JSON.
type bankReply struct {
	InvoiceID    string          `json:"invoiceId"`
	PdfURL       string          `json:"pdfUrl"`
	Status       string          `json:"status"`
	ErrorID      string          `json:"errorId"`
	ErrorMessage string          `json:"errorMessage"`
	ErrorDetails json.RawMessage `json:"errorDetails"`
}

// call — один запрос к API банка. Ошибка — только когда ответа нет;
// отказ банка — в reply и code, его разбирает bankError. Токен уходит
// только в заголовке и в тексты ошибок не попадает.
func call(ctx context.Context, method, endpoint, token string, body map[string]any) (bankReply, int, error) {
	var reply bankReply
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return reply, 0, err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return reply, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := api.Do(req)
	if err != nil {
		return reply, 0, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return reply, 0, err
	}
	// Не JSON (страница шлюза при сбое) — отказ с кодом ответа; у ответа
	// 200 без JSON не будет ни номера счёта, ни статуса, и вызывающий
	// скажет об этом сам.
	_ = json.Unmarshal(raw, &reply)
	return reply, res.StatusCode, nil
}

// bankError — отказ банка словами банка; пусто — отказа нет.
func bankError(reply bankReply, code int) string {
	if code >= 200 && code < 300 && reply.ErrorID == "" && reply.ErrorMessage == "" {
		return ""
	}
	msg := reply.ErrorMessage
	if details := strings.Trim(string(reply.ErrorDetails), `"`); details != "" && details != "null" {
		if len(details) > 300 {
			details = details[:300]
		}
		msg = strings.TrimSpace(msg + " " + details)
	}
	if msg == "" {
		msg = "код ответа " + strconv.Itoa(code)
	}
	return msg
}
