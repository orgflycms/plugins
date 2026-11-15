// Плагин «Счёт на оплату»: способ оплаты, который вместо перехода к банку
// отдаёт покупателю счёт PDF с реквизитами магазина. Деньги приходят
// переводом по счёту, оплату по выписке отмечает владелец в заказе — поэтому
// у обработчика нет ни уведомлений (Notify), ни опроса (Check), и в сеть
// плагин не ходит. База ему тоже не нужна: всё для счёта приходит в
// sdk.Payment.
package invoice

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/orgflycms/sdk"
)

// vatOptions — ставки НДС на 2026 год: 22 % — основная, 10 % — льготная,
// 5 % и 7 % — у тех, кто на УСН, 0 % — экспорт. Подпись и есть значение:
// число ставки doc.go берёт из неё.
var vatOptions = []string{"Без НДС", "НДС 0%", "НДС 5%", "НДС 7%", "НДС 10%", "НДС 22%"}

// fields — вкладка «Настройки» способа оплаты. Required — без чего счёт
// не годится в оплату: без банковских реквизитов и ИНН получателя деньги
// не отправить, и ядро прячет способ от покупателя, пока поле пусто.
var fields = []sdk.Field{
	{Key: "seller", Label: "Получатель", Required: true,
		Hint: "Название организации или ИП, как в банке: ООО «Спецодежда-Урал»."},
	{Key: "inn", Label: "ИНН", Required: true},
	{Key: "kpp", Label: "КПП", Hint: "У ИП нет — оставьте пустым."},
	{Key: "address", Label: "Адрес", Kind: "textarea", Hint: "Юридический адрес получателя для строки «Поставщик»."},
	{Key: "bank", Label: "Банк", Required: true, Hint: "Банк получателя: ПАО «Сбербанк», г. Екатеринбург."},
	{Key: "bik", Label: "БИК", Required: true},
	{Key: "corr_account", Label: "Корр. счёт", Required: true},
	{Key: "account", Label: "Расчётный счёт", Required: true},
	// НДС обязателен, а «Без НДС» — явный вариант: магазин на общей системе
	// не выставит счёт без налога по забывчивости.
	{Key: "vat", Label: "НДС", Kind: "select", Options: vatOptions, Required: true,
		Hint: "Ставка или «Без НДС» — выберите явно. НДС в счёте — «в том числе», от суммы к оплате."},
	{Key: "due_days", Label: "Срок оплаты, дней", Kind: "number",
		Hint: "Сколько дней от даты заказа счёт ждёт оплаты. Пусто — 5."},
	{Key: "director", Label: "Руководитель", Hint: "Фамилия и инициалы для подписи: Иванов И. И."},
	{Key: "accountant", Label: "Бухгалтер", Hint: "Пусто — строки подписи бухгалтера в счёте нет."},
	{Key: "note", Label: "Примечание под счётом", Kind: "textarea",
		Hint: "Условия оплаты и поставки: «Оплата этого счёта означает согласие с условиями поставки товара»."},
	{Key: "address_field", Label: "Код поля юридического адреса",
		Hint: "Поле формы заказа с юридическим адресом покупателя. Пусто — legal_address."},
}

func Init(api sdk.API) error {
	api.RegisterPayment("invoice", sdk.PaymentHandler{
		Title:    "Счёт на оплату PDF",
		Button:   "Скачать счёт",
		Settings: fields,
		Start: func(ctx context.Context, p *sdk.Payment) (sdk.PayStart, error) {
			return start(api, p)
		},
	})
	return nil
}

// start собирает счёт и отдаёт его файлом. Зовётся и для оплаченного
// заказа: бухгалтеру покупателя счёт нужен и после оплаты. Счёт каждый раз
// собирается заново, но из тех же данных — номер и дата у него от заказа,
// поэтому повторное скачивание даёт тот же документ.
func start(api sdk.API, p *sdk.Payment) (sdk.PayStart, error) {
	// Ядро прячет способ с пустым обязательным полем от новых заказов, но
	// ссылка на счёт старого заказа остаётся в письме и кабинете: счёт без
	// расчётного счёта оплатить нельзя, выдавать его незачем.
	for _, f := range fields {
		if f.Required && strings.TrimSpace(p.Settings[f.Key]) == "" {
			return sdk.PayStart{}, errors.New("Магазин ещё не заполнил реквизиты для счёта — свяжитесь с ним, пожалуйста")
		}
	}
	data, err := api.PDF(invoice(p))
	if err != nil {
		return sdk.PayStart{}, errors.New("Счёт не собрался (" + err.Error() + ") — свяжитесь с магазином, пожалуйста")
	}
	return sdk.PayStart{File: &sdk.File{Name: "Счёт " + strconv.FormatInt(p.OrderID, 10) + ".pdf", Data: data}}, nil
}
