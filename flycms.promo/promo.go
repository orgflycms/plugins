// Плагин «Промокоды»: раздел в админке, где заводят коды со скидкой от суммы
// корзины, и перехватчик корзины, который эту скидку применяет.
//
// Скидка — фиксированная сумма при корзине не меньше порога: «SALE1000 даёт
// −1 000 ₽ от 10 000 ₽». Процентные и на конкретный товар — следующий шаг,
// схема таблицы это переживёт: добавится колонка.
package promo

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/orgflycms/sdk"
)

func Init(api sdk.API) error {
	ctx := context.Background()
	if err := api.DB().Exec(ctx, `create table if not exists promo_codes (
		id        bigserial primary key,
		code      text not null unique,
		discount  bigint not null,   -- копейки
		min_total bigint not null,   -- копейки, порог суммы корзины
		active    boolean not null default true
	)`); err != nil {
		return fmt.Errorf("таблица промокодов: %w", err)
	}

	api.Menu("Промокоды", "/")
	api.Handle("GET /{$}", func(w http.ResponseWriter, r *http.Request) { list(api, w, r, "") })
	api.Handle("POST /new", func(w http.ResponseWriter, r *http.Request) { create(api, w, r) })
	api.Handle("POST /{id}/delete", func(w http.ResponseWriter, r *http.Request) {
		if err := api.DB().Exec(r.Context(), "delete from promo_codes where id = $1", r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, api.Path("/"), http.StatusSeeOther)
	})

	api.OnCart(func(ctx context.Context, c *sdk.Cart) {
		code := strings.ToUpper(strings.TrimSpace(c.Code))
		if code == "" {
			return
		}
		rows, err := api.DB().Query(ctx,
			"select discount, min_total from promo_codes where code = $1 and active", code)
		if err != nil || len(rows) == 0 {
			c.Notes = append(c.Notes, "Промокод "+code+" не найден")
			return
		}
		discount, minTotal := rows[0]["discount"].(int64), rows[0]["min_total"].(int64)
		if c.Subtotal < minTotal {
			c.Notes = append(c.Notes, fmt.Sprintf("Промокод %s действует от %s", code, api.Money(minTotal)))
			return
		}
		c.Discount += discount
		c.Notes = append(c.Notes, fmt.Sprintf("Промокод %s: −%s", code, api.Money(discount)))
	})
	return nil
}

func create(api sdk.API, w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.FormValue("code")))
	discount, ok1 := rubles(r.FormValue("discount"))
	minTotal, ok2 := rubles(r.FormValue("min_total"))
	if code == "" || !ok1 || !ok2 || discount <= 0 {
		list(api, w, r, "Нужны код, скидка больше нуля и порог — целые рубли")
		return
	}
	err := api.DB().Exec(r.Context(),
		"insert into promo_codes (code, discount, min_total) values ($1, $2, $3)", code, discount, minTotal)
	if err != nil {
		list(api, w, r, "Не сохранилось: код уже есть или база отказала")
		return
	}
	http.Redirect(w, r, api.Path("/"), http.StatusSeeOther)
}

// rubles — целые рубли из формы в копейки. Пустое поле — ноль.
func rubles(s string) (int64, bool) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if s == "" {
		return 0, true
	}
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int64(c-'0')
	}
	return n * 100, true
}

type row struct {
	ID                 any
	Code               string
	Discount, MinTotal string
}

func list(api sdk.API, w http.ResponseWriter, r *http.Request, errMsg string) {
	rows, err := api.DB().Query(r.Context(),
		"select id, code, discount, min_total from promo_codes order by id desc")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var out []row
	for _, m := range rows {
		out = append(out, row{
			ID:       m["id"],
			Code:     m["code"].(string),
			Discount: api.Money(m["discount"].(int64)),
			MinTotal: api.Money(m["min_total"].(int64)),
		})
	}

	var b strings.Builder
	err = page.Execute(&b, map[string]any{
		"Rows": out, "Error": errMsg,
		"New": api.Path("/new"), "Base": api.Path("/"),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	api.Page(w, r, "Промокоды", template.HTML(b.String()))
}

// Разметка — классы админки flycms (card, input, btn, data-table), чтобы
// раздел плагина не отличался от родных.
var page = template.Must(template.New("promo").Parse(`
<div class="card p-5 sm:p-6">
    <h2 class="text-[15px] font-semibold">Новый промокод</h2>
    {{if .Error}}<p role="alert" class="notice is-error mt-3">{{.Error}}</p>{{end}}
    <form method="post" action="{{.New}}" class="mt-4 grid gap-4 sm:grid-cols-[1fr_1fr_1fr_auto] sm:items-end">
        <label class="field"><span class="label">Код</span>
            <input name="code" class="input" placeholder="SALE1000" required></label>
        <label class="field"><span class="label">Скидка, ₽</span>
            <input name="discount" class="input" inputmode="numeric" placeholder="1000" required></label>
        <label class="field"><span class="label">От суммы, ₽</span>
            <input name="min_total" class="input" inputmode="numeric" placeholder="10000"></label>
        <button type="submit" class="btn">Добавить</button>
    </form>
</div>

<div class="card mt-5 overflow-x-auto">
    <table class="data-table">
        <thead><tr><th>Код</th><th>Скидка</th><th>От суммы</th><th></th></tr></thead>
        <tbody>
        {{range .Rows}}
            <tr>
                <td class="td-strong font-mono">{{.Code}}</td>
                <td class="td-num">−{{.Discount}}</td>
                <td class="td-num">{{.MinTotal}}</td>
                <td class="text-right">
                    <form method="post" action="{{$.Base}}{{.ID}}/delete">
                        <button type="submit" class="btn-danger">Удалить</button>
                    </form>
                </td>
            </tr>
        {{else}}
            <tr><td colspan="4" class="text-muted">Промокодов пока нет</td></tr>
        {{end}}
        </tbody>
    </table>
</div>
`))
