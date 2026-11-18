// Плагин «Т-Банк: оплата картой и СБП» — интернет-эквайринг Т-Банка.
// Покупатель уходит на платёжную форму банка, банк сообщает о деньгах
// уведомлением на адрес способа, а потерянное уведомление заменяет опрос
// GetState. Статус оплаты и заказа меняет ядро: плагин только создаёт
// платёж и пересказывает слова банка. Своей базы у плагина нет — номер
// платежа и адрес формы помнит ядро (ExternalID, ExternalURL).
package tbank

import (
	"context"
	"crypto/subtle"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/orgflycms/sdk"
)

// fields — вкладка «Настройки» способа. Ключ и пароль терминала обязательны:
// без них платёж не создать и уведомление не проверить.
var fields = []sdk.Field{
	{Key: "terminal_key", Label: "Ключ терминала (TerminalKey)", Required: true,
		Hint: "Личный кабинет Т-Бизнеса → Интернет-эквайринг → Магазины → ваш магазин → Терминалы."},
	{Key: "password", Label: "Пароль терминала", Kind: "password", Required: true,
		Hint: "Там же, рядом с ключом терминала. Им подписываются запросы и проверяются уведомления банка."},
	{Key: "receipt", Label: "Чек 54-ФЗ", Kind: "select", Options: receiptOptions,
		Hint: "«Передавать» — если к терминалу подключена онлайн-касса: состав заказа уходит в банк вместе с платежом. Пусто — не передавать."},
	{Key: "taxation", Label: "Система налогообложения", Kind: "select", Options: taxationOptions,
		Hint: "Нужна для чека: без неё при «Передавать» оплата не начнётся."},
	{Key: "vat", Label: "НДС в чеке", Kind: "select", Options: vatOptions, Hint: "Ставка всех позиций и доставки. Пусто — «Без НДС»."},
	{Key: "payment_method", Label: "Признак способа расчёта", Kind: "select", Options: methodOptions,
		Hint: "«Предоплата 100%» — если товар передаёте после оплаты и пробиваете второй чек при выдаче. Пусто — «Полный расчёт»."},
	{Key: "ffd", Label: "Формат чека", Kind: "select", Options: ffdOptions,
		Hint: "Версия ФФД вашей онлайн-кассы, видна в её настройках. Пусто — ФФД 1.05."},
}

func Init(api sdk.API) error {
	if err := initBankClients(); err != nil {
		return err
	}
	api.RegisterPayment("card", sdk.PaymentHandler{
		Title:    "Т-Банк: карта и СБП",
		Button:   "Оплатить картой или СБП",
		Settings: fields,
		Start:    start,
		Notify:   notify,
		Check:    check,
	})
	return nil
}

// Слова банка о платеже. Живой платёж ещё может быть оплачен по прежней
// ссылке; мёртвый — уже никогда, и только вместо него можно создать новый:
// иначе старый оплатят после смены номера, и ядро его не найдёт.
// Остальное (CONFIRMED, возвраты, незнакомые слова) — деньги были или
// будут, нового платежа не надо.
var (
	// Открытую форму настоящий банк называет FORM_SHOWED (с подчёркиванием);
	// FORMSHOWED — написание из старых описаний, оставлено на всякий случай.
	alive = map[string]bool{"NEW": true, "FORM_SHOWED": true, "FORMSHOWED": true, "AUTHORIZING": true, "3DS_CHECKING": true,
		"3DS_CHECKED": true, "AUTHORIZED": true, "CONFIRMING": true}
	dead = map[string]bool{"REJECTED": true, "AUTH_FAIL": true, "CANCELED": true, "DEADLINE_EXPIRED": true,
		"REVERSED": true, "ATTEMPTS_EXPIRED": true}
)

// event — слово банка в событие для ядра. Возврат (REFUNDED) — PayPending:
// оплаченное ядро оставит оплаченным, а слово покажет владельцу в заказе.
func event(m map[string]any) sdk.PayEvent {
	status := text(m, "Status")
	e := sdk.PayEvent{ExternalID: text(m, "PaymentId"), Status: sdk.PayPending, ExternalStatus: status, Reply: "OK"}
	if status == "CONFIRMED" {
		e.Status = sdk.PayPaid
		e.Amount = amount(m)
	} else if dead[status] {
		e.Status = sdk.PayFailed
	}
	return e
}

// start — покупатель нажал «Оплатить» или открыл ссылку из письма. Ссылку
// открывают сколько угодно раз, поэтому платёж в банке создаётся один:
// пока прежний жив, покупатель возвращается на его форму.
func start(ctx context.Context, p *sdk.Payment) (sdk.PayStart, error) {
	if p.Status == sdk.PayPaid {
		return sdk.PayStart{}, nil
	}
	// Способ с пустым ключом ядро от новых заказов прячет, но ссылка
	// старого заказа остаётся в письме.
	if strings.TrimSpace(p.Settings["terminal_key"]) == "" || p.Settings["password"] == "" {
		return sdk.PayStart{}, errors.New("Оплата картой сейчас не настроена — свяжитесь с магазином, пожалуйста")
	}
	orderID := strconv.FormatInt(p.ID, 10)
	if p.ExternalID != "" {
		state, err := call(ctx, p.Settings, "GetState", map[string]any{"PaymentId": p.ExternalID})
		if err != nil {
			// Что с прежним платежом — неизвестно: новый создавать нельзя.
			return sdk.PayStart{}, errors.New("Банк не ответил о начатой оплате (" + err.Error() + ") — попробуйте через минуту")
		}
		status := text(state, "Status")
		if alive[status] && p.ExternalURL != "" {
			return sdk.PayStart{ExternalID: p.ExternalID, URL: p.ExternalURL}, nil
		}
		if !dead[status] {
			return sdk.PayStart{}, nil // оплачено или возвращено: отметят уведомление или опрос
		}
		// Номер заказа у банка в терминале не повторяется — у новой
		// попытки он свой.
		orderID = retryID(p.ID)
	}

	body := map[string]any{
		"OrderId":     orderID,
		"Amount":      p.Amount,
		"Description": "Заказ №" + strconv.FormatInt(p.OrderID, 10),
		"PayType":     "O", // одна стадия: деньги списываются сразу, без подтверждения магазином
		// Форма живёт сутки: брошенный платёж банк закроет сам
		// (DEADLINE_EXPIRED), и следующий клик создаст новый. Пояс — явно
		// московский: местного пояса у плагина нет, а «Z» банк не обещает.
		"RedirectDueDate": time.Now().Add(24 * time.Hour).In(time.FixedZone("MSK", 3*60*60)).Format("2006-01-02T15:04:05-07:00"),
		"NotificationURL": p.NotifyURL,
		"SuccessURL":      p.ReturnURL,
		"FailURL":         p.ReturnURL,
	}
	r, err := receipt(p)
	if err != nil {
		return sdk.PayStart{}, err
	}
	if r != nil {
		body["Receipt"] = r
	}
	res, err := call(ctx, p.Settings, "Init", body)
	if err != nil && text(res, "ErrorCode") == "8" && orderID == strconv.FormatInt(p.ID, 10) {
		// «Заказ с таким order_id уже существует»: номер оплаты в этом
		// терминале уже занят — магазин переустановили, терминал достался от
		// прежнего сайта или прошлый ответ банка потерялся. Платёж под
		// старым номером покупателю не показан, поэтому создаём свой.
		body["OrderId"] = retryID(p.ID)
		res, err = call(ctx, p.Settings, "Init", body)
	}
	if err != nil {
		return sdk.PayStart{}, errors.New("Банк не принял платёж (" + err.Error() + ") — попробуйте ещё раз или свяжитесь с магазином")
	}
	out := sdk.PayStart{ExternalID: text(res, "PaymentId"), URL: text(res, "PaymentURL")}
	if out.ExternalID == "" || out.URL == "" || text(res, "Status") != "NEW" {
		// Номер, если он есть, ядро запомнит и с ошибкой: второй платёж
		// повторный клик не создаст.
		return out, errors.New("Банк не выдал платёжную форму — попробуйте ещё раз или свяжитесь с магазином")
	}
	return out, nil
}

// retryID — номер заказа для банка, когда простой номер оплаты занят или
// его платёж умер: в терминале номер не повторяется. Время — в
// миллисекундах: две попытки одной оплаты в одну секунду (занятый номер,
// затем отказ) с секундами получили бы один номер. Длина — не больше 33
// знаков при пределе банка 36.
func retryID(id int64) string {
	return strconv.FormatInt(id, 10) + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// notify — уведомление банка на адрес способа. Сначала подпись: без неё
// любой посторонний отметил бы заказ оплаченным. Ошибка — ядро ответит
// банку 400 и ничего не изменит.
func notify(ctx context.Context, n *sdk.Notice) (sdk.PayEvent, error) {
	m, err := decode(n.Body)
	if err != nil {
		return sdk.PayEvent{}, errors.New("уведомление не разобрано")
	}
	password := n.Settings["password"]
	if password == "" || text(m, "TerminalKey") != strings.TrimSpace(n.Settings["terminal_key"]) {
		return sdk.PayEvent{}, errors.New("уведомление чужого терминала")
	}
	got := strings.ToLower(text(m, "Token"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(token(m, password))) != 1 {
		return sdk.PayEvent{}, errors.New("неверная подпись уведомления")
	}
	e := event(m)
	if e.ExternalID == "" {
		return sdk.PayEvent{}, errors.New("в уведомлении нет номера платежа")
	}
	// Банк шлёт уведомления не по порядку: AUTHORIZED приходит через
	// миллисекунды после CONFIRMED. Ядро по контракту принимает любое слово
	// о платеже, а где оно в жизненном цикле — знает только банк и его
	// GetState (своей базы у плагина нет), поэтому о «живом» слове
	// спрашиваем, не ушёл ли платёж дальше.
	if alive[e.ExternalStatus] {
		e = latest(ctx, n.Settings, e)
	}
	return e, nil
}

// latest — событие по нынешнему состоянию платежа, если оно уже итоговое
// (CONFIRMED, возврат, отказ); иначе — слово из уведомления. Банк не
// ответил — тоже оно: уведомление нельзя терять, а оплату догонит опрос.
func latest(ctx context.Context, settings map[string]string, e sdk.PayEvent) sdk.PayEvent {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	state, err := call(ctx, settings, "GetState", map[string]any{"PaymentId": e.ExternalID})
	if st := text(state, "Status"); err != nil || st == "" || alive[st] {
		return e
	}
	now := event(state)
	now.ExternalID, now.Reply = e.ExternalID, e.Reply
	return now
}

// check — спросить банк о платеже: уведомление могло потеряться, пока
// магазин был недоступен. Ошибку видит владелец на вкладке оплат заказа.
func check(ctx context.Context, p *sdk.Payment) (sdk.PayEvent, error) {
	state, err := call(ctx, p.Settings, "GetState", map[string]any{"PaymentId": p.ExternalID})
	if err != nil {
		return sdk.PayEvent{}, errors.New("Т-Банк не сообщил статус платежа " + p.ExternalID + ": " + err.Error() +
			". Проверьте ключ и пароль терминала в настройках способа оплаты")
	}
	e := event(state)
	e.ExternalID, e.Reply = p.ExternalID, ""
	return e, nil
}
