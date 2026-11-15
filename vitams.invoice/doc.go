package invoice

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/orgflycms/sdk"
)

// Вёрстка счёта для api.PDF: банковская шапка таблицей, заголовок, стороны,
// позиции, итоги справа, сумма прописью, срок и подписи таблицей без рамок.
// Таблицы — кеглем 9: так шапка и шесть колонок позиций не теснятся.

// invoice — счёт на оплату по оплате p. Номер — номер заказа, дата — дата
// оформления заказа (Payment.Created, уже в поясе магазина): у одного
// заказа один счёт, сколько бы раз его ни скачивали.
func invoice(p *sdk.Payment) sdk.Doc {
	set := func(key string) string { return oneLine(p.Settings[key]) }
	number := strconv.FormatInt(p.OrderID, 10)
	seller := set("seller")

	// Ядро обещает: сумма строк равна сумме оплаты, скидка уже разнесена.
	// Не сошлось — одна строка на всю сумму: счёт обязан сходиться до
	// копейки, иначе бухгалтер покупателя его не оплатит.
	items := p.Items
	var total int64
	for _, it := range items {
		total += it.Sum
	}
	if total != p.Amount || len(items) == 0 {
		items = []sdk.PayItem{{Name: "Оплата заказа №" + number, Qty: 1, Price: p.Amount, Sum: p.Amount}}
		total = p.Amount
	}

	rows := [][]sdk.Cell{{
		{Text: "№", Bold: true, Align: "center"}, {Text: "Наименование", Bold: true, Align: "center"},
		{Text: "Кол-во", Bold: true, Align: "center"}, {Text: "Ед.", Bold: true, Align: "center"},
		{Text: "Цена", Bold: true, Align: "center"}, {Text: "Сумма", Bold: true, Align: "center"},
	}}
	for i, it := range items {
		rows = append(rows, []sdk.Cell{
			{Text: strconv.Itoa(i + 1), Align: "center"}, {Text: oneLine(it.Name)},
			{Text: qty(it.Qty), Align: "right"}, {Text: unit(it), Align: "center"},
			{Text: money(price(it)), Align: "right"}, {Text: money(it.Sum), Align: "right"},
		})
	}

	signs := [][]sdk.Cell{sign("Руководитель", set("director"))}
	if a := set("accountant"); a != "" {
		signs = append(signs, sign("Бухгалтер", a))
	}

	blocks := []sdk.Block{
		{Size: 9, Table: &sdk.Table{Widths: []float64{3, 3, 1, 3}, Grid: true, Rows: [][]sdk.Cell{
			{{Text: set("bank") + "\nБанк получателя", Span: 2}, {Text: "БИК\nСч. №"}, {Text: set("bik") + "\n" + set("corr_account")}},
			{{Text: "ИНН " + set("inn")}, {Text: "КПП " + set("kpp")}, {Text: "Сч. №"}, {Text: set("account")}},
			{{Text: seller + "\nПолучатель", Span: 2}, {}, {}},
		}}},
		{Text: "", Size: 4},
		{Text: "Счёт на оплату № " + number + " от " + date(p.Created), Size: 14, Bold: true},
		{Text: "Поставщик: " + join(seller, labeled("ИНН", set("inn")), labeled("КПП", set("kpp")), set("address"))},
		{Text: "Покупатель: " + buyer(p.Payer, set("address_field"))},
		{Size: 9, Table: &sdk.Table{Widths: []float64{1, 10, 2, 2, 3, 3}, Grid: true, Rows: rows}},
		{Text: "Итого: " + money(total) + " ₽", Bold: true, Align: "right"},
		{Text: vatLine(set("vat"), total), Align: "right"},
		{Text: "Всего к оплате: " + money(total) + " ₽", Bold: true, Align: "right"},
		{Text: "Всего наименований " + strconv.Itoa(len(items)) + ", на сумму " + money(total) + " руб."},
		{Text: sdk.InWords(total), Bold: true},
		{Text: "Оплатить не позднее " + date(p.Created.AddDate(0, 0, dueDays(set("due_days"))))},
	}
	// Примечание — как владелец его написал, со своими переносами строк.
	if note := strings.TrimSpace(p.Settings["note"]); note != "" {
		blocks = append(blocks, sdk.Block{Text: note})
	}
	blocks = append(blocks,
		sdk.Block{Text: ""},
		sdk.Block{Table: &sdk.Table{Widths: []float64{3, 4, 4}, Rows: signs}},
	)
	return sdk.Doc{Title: "Счёт на оплату № " + number, Blocks: blocks}
}

// buyer — строка «Покупатель»: организация с ИНН, КПП и юридическим адресом,
// у физического лица — имя из заказа. Юридического адреса среди ролей полей
// нет, он приходит в Values по коду поля из настроек.
func buyer(p sdk.Payer, addressField string) string {
	if addressField == "" {
		addressField = "legal_address"
	}
	name := oneLine(p.Company)
	if name == "" {
		name = oneLine(p.Name)
	}
	s := join(name, labeled("ИНН", oneLine(p.INN)), labeled("КПП", oneLine(p.KPP)), oneLine(p.Values[addressField]))
	if s == "" {
		return "—"
	}
	return s
}

func sign(role, name string) []sdk.Cell {
	if name != "" {
		name = "/ " + name + " /"
	}
	return []sdk.Cell{{Text: role, Bold: true}, {Text: "____________________", Align: "center"}, {Text: name}}
}

// vatLine — строка НДС под итогом. НДС «в том числе»: уже сидит в цене и к
// сумме не прибавляется, поэтому итог и «всего к оплате» сходятся всегда.
func vatLine(option string, total int64) string {
	rate, ok := vatRate(option)
	if !ok {
		return "Без налога (НДС)"
	}
	return "В том числе НДС " + strconv.Itoa(rate) + "%: " + money(vat(total, rate)) + " ₽"
}

// vatRate — ставка из подписи варианта: «НДС 22%» → 22. «Без НДС» и пусто —
// налога нет.
func vatRate(option string) (int, bool) {
	s := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(option, "НДС"), "%"))
	r, err := strconv.Atoi(s)
	return r, err == nil && r >= 0 && r < 100
}

// vat — налог, который уже сидит в сумме total: total·r/(100+r), до копейки
// по правилам арифметики (половина — вверх). Считается от суммы строк один
// раз, а не по строкам: так налог не набегает копейками округлений.
func vat(total int64, rate int) int64 {
	d := int64(100 + rate)
	return (total*int64(rate)*2 + d) / (2 * d)
}

// dueDays — срок оплаты из настройки. Пусто или не число — 5 дней.
func dueDays(v string) int {
	if v == "" {
		return 5
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64)
	if err != nil || !(f >= 0 && f < 10000) {
		return 5
	}
	return int(f)
}

// price — цена строки за единицу. Скидки на строке нет — цена из заказа как
// есть; есть — со скидкой, Sum/Qty до копейки: сумма строки остаётся точной,
// а цена × количество может разойтись с ней на копейку, как в любом счёте
// со скидкой.
func price(it sdk.PayItem) int64 {
	if it.Qty <= 0 {
		return it.Sum
	}
	if int64(math.Round(float64(it.Price)*it.Qty)) == it.Sum {
		return it.Price
	}
	return int64(math.Round(float64(it.Sum) / it.Qty))
}

// unit — единица в таблице. У доставки своей единицы нет: это услуга.
func unit(it sdk.PayItem) string {
	if it.Unit == "" && it.Delivery {
		return "усл."
	}
	return it.Unit
}

var months = []string{"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря"}

// date — «13 ноября 2026 г.».
func date(t time.Time) string {
	return strconv.Itoa(t.Day()) + " " + months[int(t.Month())-1] + " " + strconv.Itoa(t.Year()) + " г."
}

// money — копейки для документа: 2364000 → «23 640,00». Копейки пишутся
// всегда, как принято в счетах, — поэтому не api.Money, который целые рубли
// пишет без них. Знак рубля — в итогах, в ячейках таблицы его нет.
func money(k int64) string {
	sign := ""
	if k < 0 {
		sign, k = "-", -k
	}
	rub := strconv.FormatInt(k/100, 10)
	var b strings.Builder
	b.WriteString(sign)
	for i := 0; i < len(rub); i++ {
		if i > 0 && (len(rub)-i)%3 == 0 {
			b.WriteByte(' ')
		}
		b.WriteByte(rub[i])
	}
	kop := k % 100
	b.WriteByte(',')
	b.WriteByte(byte('0' + kop/10))
	b.WriteByte(byte('0' + kop%10))
	return b.String()
}

// qty — количество по-русски без лишних нулей: 2 → «2», 2.5 → «2,5».
// Три знака после запятой — точность количества в заказе (граммы, мм).
func qty(q float64) string {
	s := strconv.FormatFloat(q, 'f', 3, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	return strings.ReplaceAll(s, ".", ",")
}

// labeled — «ИНН 6600000000»; пустое значение — пустая строка, и join её
// пропустит.
func labeled(label, v string) string {
	if v == "" {
		return ""
	}
	return label + " " + v
}

// join склеивает непустые части через запятую.
func join(parts ...string) string {
	var out []string
	for _, s := range parts {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// oneLine — значение настройки или поля заказа одной строкой: адрес из
// многострочного поля, лишние пробелы.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
