package tbank

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"

	"github.com/orgflycms/sdk"
)

// Подписи полей настроек и коды Т-Банка за ними: в форме способа значение
// и подпись совпадают, поэтому владелец выбирает по-русски, а в банк уходит
// код.
var (
	receiptOptions  = []string{"Передавать", "Не передавать"}
	ffdOptions      = []string{"ФФД 1.05", "ФФД 1.2"}
	taxationOptions = []string{"ОСН", "УСН доходы", "УСН доходы минус расходы", "ЕСХН", "Патент"}
	taxationCodes   = map[string]string{"ОСН": "osn", "УСН доходы": "usn_income",
		"УСН доходы минус расходы": "usn_income_outcome", "ЕСХН": "esn", "Патент": "patent"}
	// Ставки НДС на 2026 год: 22 % — основная, 10 % — льготная, 5 % и 7 % —
	// у тех, кто на УСН, 0 % — экспорт.
	vatOptions = []string{"Без НДС", "НДС 0%", "НДС 5%", "НДС 7%", "НДС 10%", "НДС 22%"}
	vatCodes   = map[string]string{"Без НДС": "none", "НДС 0%": "vat0", "НДС 5%": "vat5",
		"НДС 7%": "vat7", "НДС 10%": "vat10", "НДС 22%": "vat22"}
	methodOptions = []string{"Полный расчёт", "Предоплата 100%"}
	methodCodes   = map[string]string{"Полный расчёт": "full_payment", "Предоплата 100%": "full_prepayment"}
)

// receipt — чек 54-ФЗ для Init. nil без ошибки — владелец чек не передаёт.
// Ошибка — текст для покупателя: без системы налогообложения или контакта
// банк чек не примет, и платёж не создастся.
func receipt(p *sdk.Payment) (map[string]any, error) {
	if p.Settings["receipt"] != "Передавать" {
		return nil, nil
	}
	taxation := taxationCodes[p.Settings["taxation"]]
	if taxation == "" {
		return nil, errors.New("Магазин ещё не настроил чек для оплаты картой — свяжитесь с ним, пожалуйста")
	}
	out := map[string]any{"Taxation": taxation}
	// Чек покупателю банк шлёт на почту или телефон: одно из двух обязано быть.
	email, phone := strings.TrimSpace(p.Payer.Email), receiptPhone(p.Payer.Phone)
	if email != "" {
		out["Email"] = email
	}
	if phone != "" {
		out["Phone"] = phone
	}
	if email == "" && phone == "" {
		return nil, errors.New("Для чека нужны почта или телефон, а в заказе их нет — свяжитесь с магазином, пожалуйста")
	}
	// Формат банк при Init не сверяет (проба на тестовом терминале
	// 2026-11-15: чек принят и с FfdVersion, и без, и с MeasurementUnit, и
	// без) — его проверяет касса, когда пробивает чек после списания.
	ffd12 := p.Settings["ffd"] == "ФФД 1.2"
	if ffd12 {
		out["FfdVersion"] = "1.2"
	}
	tax := vatCodes[p.Settings["vat"]]
	if tax == "" {
		tax = "none"
	}
	method := methodCodes[p.Settings["payment_method"]]
	if method == "" {
		method = "full_payment"
	}
	items := []any{}
	for _, it := range p.Items {
		object := "commodity"
		if it.Delivery {
			object = "service"
		}
		for _, l := range receiptLines(it) {
			item := map[string]any{"Name": l.name, "Price": l.price, "Quantity": json.Number(l.qty), "Amount": l.amount,
				"Tax": tax, "PaymentMethod": method, "PaymentObject": object}
			if ffd12 {
				item["MeasurementUnit"] = l.unit
			}
			items = append(items, item)
		}
	}
	out["Items"] = items
	return out, nil
}

// line — позиция чека: банк сверяет amount == price × qty в каждой.
type line struct {
	name   string
	unit   string
	qty    string // количество десятичной записью: «2», «2.5»
	price  int64
	amount int64
}

// receiptLines раскладывает строку заказа на позиции чека так, чтобы в
// каждой цена на количество давала стоимость до копейки, а сумма позиций
// осталась равна Sum строки. Скидка заказа уже разнесена ядром по строкам,
// поэтому Sum на количество делится не всегда:
//
//   - 3 шт на 1 000,00 ₽ — две позиции: 2 × 333,33 и 1 × 333,34;
//   - 2,5 м на 730,17 ₽ — цена за метр не целая в копейках: одна позиция
//     «Ткань, 2,5 м» количеством 1 на всю сумму.
//
// Строки с нулевой суммой (подарок, скидка на всю цену) в чек не идут: на
// итог они не влияют, а позицию с нулевой ценой банк может не принять.
func receiptLines(it sdk.PayItem) []line {
	if it.Sum <= 0 {
		return nil
	}
	name := receiptName(it.Name)
	unit := strings.TrimSpace(it.Unit)
	if unit == "" {
		unit = "шт"
	}
	// Количество в тысячных: точнее трёх знаков каталог его не хранит.
	milli := int64(math.Round(it.Qty * 1000))
	if milli <= 0 || math.Abs(it.Qty*1000-float64(milli)) > 1e-6 {
		return []line{{name: name, unit: "шт", qty: "1", price: it.Sum, amount: it.Sum}}
	}
	if milli%1000 != 0 {
		price := int64(math.Round(float64(it.Sum) * 1000 / float64(milli)))
		if price*milli == it.Sum*1000 {
			return []line{{name: name, unit: unit, qty: decimal(milli), price: price, amount: it.Sum}}
		}
		return []line{{name: receiptName(it.Name + ", " + strings.Replace(decimal(milli), ".", ",", 1) + " " + unit),
			unit: "шт", qty: "1", price: it.Sum, amount: it.Sum}}
	}
	n := milli / 1000
	price := it.Sum / n
	if price*n == it.Sum {
		return []line{{name: name, unit: unit, qty: strconv.FormatInt(n, 10), price: price, amount: it.Sum}}
	}
	// Остаток от деления уходит в последнюю штуку.
	rest := it.Sum - price*(n-1)
	return []line{
		{name: name, unit: unit, qty: strconv.FormatInt(n-1, 10), price: price, amount: price * (n - 1)},
		{name: name, unit: unit, qty: "1", price: rest, amount: rest},
	}
}

// decimal — тысячные десятичной записью без хвостовых нулей: 2500 → «2.5».
func decimal(milli int64) string {
	s := strconv.FormatInt(milli/1000, 10)
	if frac := milli % 1000; frac != 0 {
		f := strconv.FormatInt(1000+frac, 10)[1:]
		s += "." + strings.TrimRight(f, "0")
	}
	return s
}

// receiptName — наименование позиции: банк принимает не больше 128 знаков.
func receiptName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Товар"
	}
	if r := []rune(name); len(r) > 128 {
		return string(r[:128])
	}
	return name
}

// receiptPhone — телефон для чека в виде +79000000000; не похож на
// российский номер — пусто, чек уйдёт на почту.
func receiptPhone(phone string) string {
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, phone)
	if len(digits) == 10 {
		digits = "7" + digits
	}
	if len(digits) == 11 && digits[0] == '8' {
		digits = "7" + digits[1:]
	}
	if len(digits) != 11 || digits[0] != '7' {
		return ""
	}
	return "+" + digits
}
