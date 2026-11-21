// Плагин «СДЭК»: два способа доставки (курьер до двери и пункт выдачи)
// и регистрация заказа в СДЭК после оплаты. Цену и срок считает API по
// коду тарифа, в заказ их пишет ядро. Номер отправления контракт в
// отгрузку положить не даёт — он остаётся в таблице плагина и в разделе
// «СДЭК».
package cdek

import (
	"context"
	"errors"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/orgflycms/sdk"
)

const (
	tariffDoor  = 137 // посылка склад-дверь
	tariffPoint = 136 // посылка склад-склад
)

var (
	pluginName string
	db         sdk.DB

	cityMu    sync.Mutex
	cityCache = map[string]int{}
)

func fields() []sdk.Field {
	return []sdk.Field{
		{Key: "account", Label: "Идентификатор (Account)", Required: true,
			Hint: "Личный кабинет СДЭК → Интеграция. Учебная учётка — на странице протокола API, она работает только со средой «учебная»."},
		{Key: "secure", Label: "Пароль (Secure)", Kind: "password", Required: true},
		{Key: "test", Label: "Среда", Kind: "select", Options: []string{"учебная", "боевая"}, Required: true,
			Hint: "«учебная» — api.edu.cdek.ru, посылки не везут. «боевая» — настоящие заказы на api.cdek.ru."},
		{Key: "from_city", Label: "Город отправки", Required: true,
			Hint: "Название или код города СДЭК: Москва или 44."},
		{Key: "from_address", Label: "Адрес отправки",
			Hint: "Улица и дом, если курьер забирает у вас. Для отправки со склада можно пусто."},
		{Key: "shipment_point", Label: "Пункт приёма",
			Hint: "Код пункта СДЭК, откуда забирают посылки. Пусто — отправка из города."},
		{Key: "tariff", Label: "Код тарифа", Kind: "number",
			Hint: "Пусто — 137 (склад-дверь) у курьера и 136 (склад-склад) у пункта. Коды — в приложении к API СДЭК."},
		{Key: "weight_g", Label: "Вес по умолчанию, г", Kind: "number",
			Hint: "Для товара, у которого вес не указан. Пусто — 1000."},
		{Key: "length_mm", Label: "Длина по умолчанию, мм", Kind: "number", Hint: "Пусто — 200."},
		{Key: "width_mm", Label: "Ширина по умолчанию, мм", Kind: "number", Hint: "Пусто — 150."},
		{Key: "height_mm", Label: "Высота по умолчанию, мм", Kind: "number", Hint: "Пусто — 100."},
		{Key: "sender_name", Label: "Отправитель", Required: true, Hint: "Имя в заказе СДЭК."},
		{Key: "sender_phone", Label: "Телефон отправителя", Required: true},
		{Key: "on_status", Label: "Создавать заказ при статусе",
			Hint: "Код статуса магазина. Пусто — когда заказ оплачен."},
	}
}

func Init(api sdk.API) error {
	pluginName = api.Name()
	db = api.DB()
	if err := db.Exec(context.Background(), `create table if not exists cdek_orders (
		order_id    bigint primary key,
		uuid        text not null default '',
		cdek_number text not null default '',
		error       text not null default ''
	)`); err != nil {
		return err
	}
	api.Menu("СДЭК", "/")
	api.Handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) { page(api, w, r) })
	api.RegisterDelivery("door", sdk.DeliveryHandler{
		Title: "СДЭК: курьер до двери", Settings: fields(), Quote: func(ctx context.Context, r *sdk.DeliveryRequest) (sdk.DeliveryQuote, error) {
			return quote(ctx, r, tariffDoor, false)
		},
	})
	api.RegisterDelivery("point", sdk.DeliveryHandler{
		Title: "СДЭК: до пункта выдачи", Settings: fields(), Quote: func(ctx context.Context, r *sdk.DeliveryRequest) (sdk.DeliveryQuote, error) {
			return quote(ctx, r, tariffPoint, true)
		},
	})
	api.OnOrder(onOrder)
	return nil
}

func quote(ctx context.Context, r *sdk.DeliveryRequest, defTariff int, point bool) (sdk.DeliveryQuote, error) {
	s := r.Settings
	if s == nil {
		s = map[string]string{}
	}
	if strings.TrimSpace(s["account"]) == "" || s["secure"] == "" {
		return sdk.DeliveryQuote{}, errors.New("СДЭК не настроен")
	}
	if r.Place.ID == 0 && strings.TrimSpace(r.Place.Name) == "" && strings.TrimSpace(r.Zip) == "" {
		return sdk.DeliveryQuote{Missing: "Укажите город"}, nil
	}
	from, err := cityCode(ctx, s, s["from_city"], nil)
	if err != nil {
		return sdk.DeliveryQuote{}, err
	}
	to, err := cityCode(ctx, s, r.Place.Name, r.Place.Path)
	if err != nil {
		if errors.Is(err, errNoCity) {
			return sdk.DeliveryQuote{Missing: "Этот город СДЭК не обслуживает"}, nil
		}
		return sdk.DeliveryQuote{}, err
	}
	if code := strings.TrimSpace(r.Place.Code); code != "" && !strings.Contains(code, "-") {
		if n, conv := strconv.Atoi(code); conv == nil && n > 0 {
			to = n
		}
	}
	if point && strings.TrimSpace(r.PickupPoint) == "" {
		return sdk.DeliveryQuote{Missing: pointHint(ctx, s, to)}, nil
	}
	w, l, wd, h := pack(r.Items, s)
	body := map[string]any{
		"tariff_code":   tariff(s, defTariff),
		"from_location": map[string]any{"code": from},
		"to_location":   map[string]any{"code": to},
		"packages":      []any{map[string]any{"weight": w, "length": l, "width": wd, "height": h}},
	}
	code, v, err := call(ctx, s, http.MethodPost, "/v2/calculator/tariff", body, true)
	if err != nil {
		return sdk.DeliveryQuote{}, err
	}
	m := obj(v)
	sum, ok := asFloat(m["total_sum"])
	if !ok || code >= 400 {
		// total_sum — с услугами и НДС: это сумма, которую выставит СДЭК.
		// delivery_sum её не содержит.
		if alt, ok2 := asFloat(m["delivery_sum"]); ok2 && code < 400 {
			sum, ok = alt, true
		}
	}
	if !ok || code >= 400 {
		msg := cdekMessage(v)
		if msg == "" {
			msg = "код " + strconv.Itoa(code)
		}
		return sdk.DeliveryQuote{}, errors.New("СДЭК не рассчитал доставку: " + msg)
	}
	kop := int64(math.Round(sum * 100))
	minD, _ := asInt(m["period_min"])
	maxD, _ := asInt(m["period_max"])
	if minD < 0 {
		minD = 0
	}
	if maxD < minD {
		maxD = minD
	}
	return sdk.DeliveryQuote{Price: kop, MinDays: minD, MaxDays: maxD}, nil
}

var errNoCity = errors.New("город не найден")

// cityCode — код населённого пункта СДЭК. Цифры в настройке — уже код.
// Иначе ищем по имени: при нескольких совпадениях берём тот, чей регион
// есть в пути местоположения, затем город федерального значения.
func cityCode(ctx context.Context, s map[string]string, name string, path []string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errNoCity
	}
	if n, err := strconv.Atoi(name); err == nil && n > 0 {
		return n, nil
	}
	key := baseURL(s) + "\n" + strings.ToLower(name)
	cityMu.Lock()
	if c, ok := cityCache[key]; ok {
		cityMu.Unlock()
		return c, nil
	}
	cityMu.Unlock()

	q := "/v2/location/cities?country_codes=RU&size=20&city=" + url.QueryEscape(name)
	code, v, err := call(ctx, s, http.MethodGet, q, nil, true)
	if err != nil {
		return 0, err
	}
	if code >= 400 {
		msg := cdekMessage(v)
		if msg == "" {
			msg = "код " + strconv.Itoa(code)
		}
		return 0, errors.New("СДЭК не нашёл город: " + msg)
	}
	var exact []cityHit
	for _, e := range list(v) {
		m := obj(e)
		n, _ := m["city"].(string)
		c, ok := asInt(m["code"])
		if !ok || !strings.EqualFold(strings.TrimSpace(n), name) {
			continue
		}
		region, _ := m["region"].(string)
		exact = append(exact, cityHit{c, region})
	}
	if len(exact) == 0 {
		return 0, errNoCity
	}
	chosen := pickCity(exact, path)
	cityMu.Lock()
	cityCache[key] = chosen
	cityMu.Unlock()
	return chosen, nil
}

type cityHit struct {
	code   int
	region string
}

func pickCity(exact []cityHit, path []string) int {
	for _, c := range exact {
		for _, p := range path {
			if strings.EqualFold(strings.TrimSpace(c.region), strings.TrimSpace(p)) {
				return c.code
			}
		}
	}
	return exact[0].code
}

func pointHint(ctx context.Context, s map[string]string, city int) string {
	const head = "Укажите код пункта выдачи СДЭК"
	q := "/v2/deliverypoints?type=PVZ&size=5&city_code=" + strconv.Itoa(city)
	code, v, err := call(ctx, s, http.MethodGet, q, nil, false)
	if err != nil || code >= 400 {
		return head
	}
	var bits []string
	for _, e := range list(v) {
		m := obj(e)
		id, _ := m["code"].(string)
		if id == "" {
			continue
		}
		addr, _ := obj(m["location"])["address"].(string)
		if addr == "" {
			addr, _ = m["name"].(string)
		}
		bits = append(bits, id+" — "+addr)
		if len(bits) == 5 {
			break
		}
	}
	if len(bits) == 0 {
		return head
	}
	return head + ": " + strings.Join(bits, "; ")
}

func tariff(s map[string]string, def int) int {
	return settingInt(s, "tariff", def)
}

func doorOrPoint(point bool) int {
	if point {
		return tariffPoint
	}
	return tariffDoor
}

func settingInt(s map[string]string, key string, def int) int {
	v := strings.ReplaceAll(strings.TrimSpace(s[key]), " ", "")
	v = strings.ReplaceAll(v, ",", ".")
	if v == "" {
		return def
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 || n > 1e9 {
		return def
	}
	return int(n)
}

// pack — одно место: вес суммы строк (у строки без веса — вес по
// умолчанию), габариты — наибольшие указанные, иначе из настроек.
// СДЭК ждёт вес в граммах и размеры в сантиметрах.
func pack(items []sdk.DeliveryItem, s map[string]string) (weight, l, w, h int) {
	defW := settingInt(s, "weight_g", 1000)
	l, w, h = settingInt(s, "length_mm", 200), settingInt(s, "width_mm", 150), settingInt(s, "height_mm", 100)
	var sum float64
	var saw bool
	for _, it := range items {
		q := it.Qty
		if q <= 0 {
			q = 1
		}
		gw := it.WeightG
		if gw <= 0 {
			gw = defW
		}
		sum += float64(gw) * q
		if it.LengthMM > l {
			l = it.LengthMM
		}
		if it.WidthMM > w {
			w = it.WidthMM
		}
		if it.HeightMM > h {
			h = it.HeightMM
		}
		saw = true
	}
	if !saw || sum < 1 {
		sum = float64(defW)
	}
	weight = int(math.Round(sum))
	if weight < 1 {
		weight = 1
	}
	return weight, cm(l), cm(w), cm(h)
}

func cm(mm int) int {
	if mm <= 0 {
		return 1
	}
	c := (mm + 9) / 10
	if c < 1 {
		return 1
	}
	return c
}

func onOrder(ctx context.Context, e *sdk.OrderEvent) error {
	d := e.Order.Delivery
	if d.Handler != pluginName+"/door" && d.Handler != pluginName+"/point" {
		return nil
	}
	if d.Settings == nil {
		return nil // способ сменили или удалили — ключей нет, повтор ничего не даст
	}
	uuid, number, err := load(ctx, e.Order.ID)
	if err != nil {
		return err
	}
	if number != "" {
		return nil
	}
	if uuid == "" && !wantCreate(e, d.Settings) {
		return nil
	}
	uuid, number, err = ensure(ctx, e, uuid)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if saveErr := save(ctx, e.Order.ID, uuid, number, msg); saveErr != nil {
		return saveErr
	}
	if err != nil {
		return err
	}
	if number == "" {
		// Повтор того же события спросит номер ещё раз: СДЭК присваивает его не сразу.
		return errors.New("СДЭК принял заказ, номер ещё не присвоен")
	}
	return nil
}

func wantCreate(e *sdk.OrderEvent, s map[string]string) bool {
	code := strings.TrimSpace(s["on_status"])
	if code != "" {
		return e.Kind == sdk.OrderStatus && e.Status == code
	}
	return e.Kind == sdk.OrderPaid || (e.Kind == sdk.OrderStatus && e.Status == "paid")
}

func load(ctx context.Context, order int64) (uuid, number string, err error) {
	rows, err := db.Query(ctx, "select uuid, cdek_number from cdek_orders where order_id = $1", order)
	if err != nil || len(rows) == 0 {
		return "", "", err
	}
	uuid, _ = rows[0]["uuid"].(string)
	number, _ = rows[0]["cdek_number"].(string)
	return uuid, number, nil
}

func save(ctx context.Context, order int64, uuid, number, msg string) error {
	return db.Exec(ctx, `insert into cdek_orders (order_id, uuid, cdek_number, error) values ($1, $2, $3, $4)
		on conflict (order_id) do update set uuid = excluded.uuid, cdek_number = excluded.cdek_number, error = excluded.error`,
		order, uuid, number, msg)
}

// ensure — один заказ СДЭК на заказ магазина. Номер магазина — номер
// заказа: перед созданием ищем его, чтобы повтор события не завёл второе
// отправление, если ответ на создание потерялся.
func ensure(ctx context.Context, e *sdk.OrderEvent, uuid string) (string, string, error) {
	s := e.Order.Delivery.Settings
	im := strconv.FormatInt(e.Order.ID, 10)
	if uuid == "" {
		u, n, err := findByNumber(ctx, s, im)
		if err != nil || n != "" || u != "" {
			return u, n, err
		}
		var errCreate error
		uuid, errCreate = create(ctx, e)
		if errCreate != nil {
			u, n, findErr := findByNumber(ctx, s, im)
			if findErr == nil && (u != "" || n != "") {
				return u, n, nil
			}
			return "", "", errCreate
		}
	}
	u, n, state, msg, err := fetch(ctx, s, uuid)
	if err != nil {
		return uuid, "", err
	}
	if u != "" {
		uuid = u
	}
	if state == "INVALID" {
		if strings.Contains(strings.ToLower(msg), "not unique") || strings.Contains(msg, "уже существует") {
			return findByNumber(ctx, s, im)
		}
		if msg == "" {
			msg = "СДЭК отклонил заказ"
		}
		return uuid, "", errors.New(msg)
	}
	return uuid, n, nil
}

func findByNumber(ctx context.Context, s map[string]string, im string) (string, string, error) {
	code, v, err := call(ctx, s, http.MethodGet, "/v2/orders?im_number="+url.QueryEscape(im), nil, true)
	if err != nil {
		return "", "", err
	}
	if code == http.StatusNotFound {
		return "", "", nil
	}
	if code >= 400 {
		return "", "", errors.New(refusal(v, code))
	}
	u, n, state, msg, _ := readOrder(v)
	if state == "INVALID" {
		return "", "", errors.New(first(msg, "СДЭК отклонил заказ"))
	}
	return u, n, nil
}

func fetch(ctx context.Context, s map[string]string, uuid string) (string, string, string, string, error) {
	code, v, err := call(ctx, s, http.MethodGet, "/v2/orders/"+url.PathEscape(uuid), nil, true)
	if err != nil {
		return uuid, "", "", "", err
	}
	if code >= 400 {
		return uuid, "", "", "", errors.New(refusal(v, code))
	}
	return readOrder(v)
}

func readOrder(v any) (uuid, number, state, msg string, err error) {
	m := obj(v)
	ent := obj(m["entity"])
	uuid, _ = ent["uuid"].(string)
	number, _ = ent["cdek_number"].(string)
	msg = cdekMessage(v)
	for _, r := range list(m["requests"]) {
		st, _ := obj(r)["state"].(string)
		if st != "" {
			state = st
		}
	}
	return uuid, number, state, msg, nil
}

func refusal(v any, code int) string {
	msg := cdekMessage(v)
	if msg == "" {
		return "СДЭК ответил кодом " + strconv.Itoa(code)
	}
	return msg
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func create(ctx context.Context, e *sdk.OrderEvent) (string, error) {
	s := e.Order.Delivery.Settings
	o := e.Order
	point := strings.HasSuffix(o.Delivery.Handler, "/point")
	from, err := cityCode(ctx, s, s["from_city"], nil)
	if err != nil {
		return "", err
	}
	phone := normalizePhone(s["sender_phone"])
	if strings.TrimSpace(s["sender_name"]) == "" || phone == "" {
		return "", errors.New("В настройках СДЭК не заполнены отправитель и его телефон")
	}
	toPhone := normalizePhone(o.Phone)
	if toPhone == "" {
		return "", errors.New("В заказе нет телефона — СДЭК без него заказ не примет")
	}
	body := map[string]any{
		"type":        1,
		"number":      strconv.FormatInt(o.ID, 10),
		"tariff_code": tariff(s, doorOrPoint(point)),
		"sender":      map[string]any{"name": strings.TrimSpace(s["sender_name"]), "phones": []any{map[string]any{"number": phone}}},
		"recipient":   recipient(o, toPhone),
		"packages":    []any{parcel(o, s)},
	}
	if note := strings.TrimSpace(o.Note); note != "" {
		body["comment"] = trim(note, 200)
	}
	if p := strings.TrimSpace(s["shipment_point"]); p != "" {
		body["shipment_point"] = p
	} else {
		fromLoc := map[string]any{"code": from}
		if a := strings.TrimSpace(s["from_address"]); a != "" {
			fromLoc["address"] = a
		}
		body["from_location"] = fromLoc
	}
	if point {
		body["delivery_point"] = strings.TrimSpace(o.Delivery.PickupPoint)
	} else {
		to, err := cityCode(ctx, s, o.Delivery.Place.Name, o.Delivery.Place.Path)
		if err != nil {
			return "", err
		}
		toLoc := map[string]any{"code": to}
		if a := strings.TrimSpace(o.Delivery.Address); a != "" {
			toLoc["address"] = a
		}
		if z := strings.TrimSpace(o.Delivery.Zip); z != "" {
			toLoc["postal_code"] = z
		}
		body["to_location"] = toLoc
	}
	// Повтор POST не делаем: при обрыве следующее событие найдёт заказ по номеру.
	code, v, err := call(ctx, s, http.MethodPost, "/v2/orders", body, false)
	if err != nil {
		return "", err
	}
	u, _, state, msg, _ := readOrder(v)
	if code >= 400 || state == "INVALID" || u == "" {
		return "", errors.New(first(msg, "СДЭК не принял заказ, код "+strconv.Itoa(code)))
	}
	return u, nil
}

func recipient(o sdk.Order, phone string) map[string]any {
	name := strings.TrimSpace(o.Customer)
	if name == "" {
		name = "Покупатель"
	}
	r := map[string]any{"name": name, "phones": []any{map[string]any{"number": phone}}}
	if emailOK(o.Email) {
		r["email"] = strings.TrimSpace(o.Email)
	}
	return r
}

func parcel(o sdk.Order, s map[string]string) map[string]any {
	w, l, wd, h := pack(orderAsItems(o.Items), s)
	var items []any
	kids := map[int64]bool{}
	for _, it := range o.Items {
		if it.ParentID != 0 {
			kids[it.ParentID] = true
		}
	}
	for _, it := range o.Items {
		if it.ParentID != 0 || kids[it.ID] && it.Sum == 0 {
			continue
		}
		if kids[it.ID] {
			// Комплект: деньги на строке комплекта, вес — на составе.
			items = append(items, cdekItem(it, componentWeight(o.Items, it.ID, s)))
			continue
		}
		items = append(items, cdekItem(it, 0))
	}
	if len(items) == 0 {
		items = append(items, map[string]any{
			"name": "Заказ", "ware_key": "order", "payment": map[string]any{"value": 0},
			"cost": 0, "weight": w, "amount": 1,
		})
	}
	return map[string]any{"number": "1", "weight": w, "length": l, "width": wd, "height": h, "items": items}
}

func orderAsItems(items []sdk.OrderItem) []sdk.DeliveryItem {
	var out []sdk.DeliveryItem
	for _, it := range items {
		if it.ParentID != 0 {
			out = append(out, sdk.DeliveryItem{Name: it.Name, Qty: it.Qty, Price: it.Price, WeightG: it.WeightG,
				LengthMM: it.LengthMM, WidthMM: it.WidthMM, HeightMM: it.HeightMM})
		}
	}
	if len(out) == 0 {
		for _, it := range items {
			out = append(out, sdk.DeliveryItem{Name: it.Name, Qty: it.Qty, Price: it.Price, WeightG: it.WeightG,
				LengthMM: it.LengthMM, WidthMM: it.WidthMM, HeightMM: it.HeightMM})
		}
	}
	return out
}

func componentWeight(items []sdk.OrderItem, parent int64, s map[string]string) int {
	var sum float64
	for _, it := range items {
		if it.ParentID != parent {
			continue
		}
		q := it.Qty
		if q <= 0 {
			q = 1
		}
		w := it.WeightG
		if w <= 0 {
			w = settingInt(s, "weight_g", 1000)
		}
		sum += float64(w) * q
	}
	if sum < 1 {
		return settingInt(s, "weight_g", 1000)
	}
	return int(math.Round(sum))
}

func cdekItem(it sdk.OrderItem, weight int) map[string]any {
	q := it.Qty
	if q <= 0 {
		q = 1
	}
	amount := 1
	per := it.WeightG
	name := it.Name
	if n, ok := whole(q); ok && weight == 0 {
		amount = n
	} else {
		if weight == 0 {
			if per <= 0 {
				per = 1
			}
			weight = int(math.Round(float64(per) * q))
		}
		if amount == 1 && q != 1 {
			name = trim(name+" ("+strconv.FormatFloat(q, 'f', -1, 64)+" "+it.Unit+")", 255)
		}
		per = weight
	}
	if per <= 0 {
		per = weight
	}
	if per <= 0 {
		per = 1
	}
	key := strings.TrimSpace(it.Article)
	if key == "" {
		key = strconv.FormatInt(it.OfferID, 10)
	}
	if key == "" || key == "0" {
		key = strconv.FormatInt(it.ID, 10)
	}
	cost := float64(it.Sum) / 100
	if cost < 0 {
		cost = 0
	}
	return map[string]any{
		"name": name, "ware_key": trim(key, 50), "payment": map[string]any{"value": 0},
		"cost": cost, "weight": per, "amount": amount,
	}
}

func whole(q float64) (int, bool) {
	if q < 1 || q > 9999 {
		return 0, false
	}
	n := int(math.Round(q))
	if math.Abs(q-float64(n)) > 0.001 {
		return 0, false
	}
	return n, true
}

func normalizePhone(s string) string {
	var d []byte
	for _, r := range s {
		if r >= '0' && r <= '9' {
			d = append(d, byte(r))
		}
	}
	if len(d) == 11 && d[0] == '8' {
		d[0] = '7'
	}
	if len(d) == 10 {
		d = append([]byte{'7'}, d...)
	}
	if len(d) < 11 {
		return ""
	}
	return "+" + string(d)
}

func emailOK(s string) bool {
	s = strings.TrimSpace(s)
	at := strings.Index(s, "@")
	return at > 0 && strings.Contains(s[at+1:], ".") && !strings.ContainsAny(s, " \t")
}

func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

type shipRow struct {
	Order        int64
	UUID, Number string
	Error        string
}

func page(api sdk.API, w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(r.Context(), `select order_id, uuid, cdek_number, error from cdek_orders order by order_id desc limit 50`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var out []shipRow
	for _, m := range rows {
		id, _ := m["order_id"].(int64)
		u, _ := m["uuid"].(string)
		n, _ := m["cdek_number"].(string)
		e, _ := m["error"].(string)
		out = append(out, shipRow{Order: id, UUID: u, Number: n, Error: e})
	}
	var b strings.Builder
	if err := shipPage.Execute(&b, out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	api.Page(w, r, "СДЭК", template.HTML(b.String()))
}

var shipPage = template.Must(template.New("cdek").Parse(`
<div class="card overflow-x-auto">
    <table class="data-table">
        <thead><tr><th>Заказ</th><th>Номер СДЭК</th><th>UUID</th><th>Ошибка</th></tr></thead>
        <tbody>
        {{range .}}
            <tr>
                <td class="td-strong">{{.Order}}</td>
                <td class="font-mono">{{.Number}}</td>
                <td class="font-mono text-muted">{{.UUID}}</td>
                <td>{{.Error}}</td>
            </tr>
        {{else}}
            <tr><td colspan="4" class="text-muted">Отправлений пока нет. Заказ уходит в СДЭК после оплаты или при статусе из настроек способа.</td></tr>
        {{end}}
        </tbody>
    </table>
</div>
`))
