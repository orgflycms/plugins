package tbank_invoice

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// mincifryRootPEM — корневой сертификат НУЦ Минцифры (Russian Trusted Root
// CA, действует до 27.02.2032, SHA-256 D2:6D:2D:02:…:CA:8E:CF:31).
// business.tbank.ru отдаёт PDF счёта с цепочкой этого корня, а в обычном
// хранилище сервера его нет. Строкой, а не файлом: плагину файлы закрыты,
// и архив для маркетплейса принимает только *.go.
const mincifryRootPEM = `-----BEGIN CERTIFICATE-----
MIIFwjCCA6qgAwIBAgICEAAwDQYJKoZIhvcNAQELBQAwcDELMAkGA1UEBhMCUlUx
PzA9BgNVBAoMNlRoZSBNaW5pc3RyeSBvZiBEaWdpdGFsIERldmVsb3BtZW50IGFu
ZCBDb21tdW5pY2F0aW9uczEgMB4GA1UEAwwXUnVzc2lhbiBUcnVzdGVkIFJvb3Qg
Q0EwHhcNMjIwMzAxMjEwNDE1WhcNMzIwMjI3MjEwNDE1WjBwMQswCQYDVQQGEwJS
VTE/MD0GA1UECgw2VGhlIE1pbmlzdHJ5IG9mIERpZ2l0YWwgRGV2ZWxvcG1lbnQg
YW5kIENvbW11bmljYXRpb25zMSAwHgYDVQQDDBdSdXNzaWFuIFRydXN0ZWQgUm9v
dCBDQTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAMfFOZ8pUAL3+r2n
qqE0Zp52selXsKGFYoG0GM5bwz1bSFtCt+AZQMhkWQheI3poZAToYJu69pHLKS6Q
XBiwBC1cvzYmUYKMYZC7jE5YhEU2bSL0mX7NaMxMDmH2/NwuOVRj8OImVa5s1F4U
zn4Kv3PFlDBjjSjXKVY9kmjUBsXQrIHeaqmUIsPIlNWUnimXS0I0abExqkbdrXbX
YwCOXhOO2pDUx3ckmJlCMUGacUTnylyQW2VsJIyIGA8V0xzdaeUXg0VZ6ZmNUr5Y
Ber/EAOLPb8NYpsAhJe2mXjMB/J9HNsoFMBFJ0lLOT/+dQvjbdRZoOT8eqJpWnVD
U+QL/qEZnz57N88OWM3rabJkRNdU/Z7x5SFIM9FrqtN8xewsiBWBI0K6XFuOBOTD
4V08o4TzJ8+Ccq5XlCUW2L48pZNCYuBDfBh7FxkB7qDgGDiaftEkZZfApRg2E+M9
G8wkNKTPLDc4wH0FDTijhgxR3Y4PiS1HL2Zhw7bD3CbslmEGgfnnZojNkJtcLeBH
BLa52/dSwNU4WWLubaYSiAmA9IUMX1/RpfpxOxd4Ykmhz97oFbUaDJFipIggx5sX
ePAlkTdWnv+RWBxlJwMQ25oEHmRguNYf4Zr/Rxr9cS93Y+mdXIZaBEE0KS2iLRqa
OiWBki9IMQU4phqPOBAaG7A+eP8PAgMBAAGjZjBkMB0GA1UdDgQWBBTh0YHlzlpf
BKrS6badZrHF+qwshzAfBgNVHSMEGDAWgBTh0YHlzlpfBKrS6badZrHF+qwshzAS
BgNVHRMBAf8ECDAGAQH/AgEEMA4GA1UdDwEB/wQEAwIBhjANBgkqhkiG9w0BAQsF
AAOCAgEAALIY1wkilt/urfEVM5vKzr6utOeDWCUczmWX/RX4ljpRdgF+5fAIS4vH
tmXkqpSCOVeWUrJV9QvZn6L227ZwuE15cWi8DCDal3Ue90WgAJJZMfTshN4OI8cq
W9E4EG9wglbEtMnObHlms8F3CHmrw3k6KmUkWGoa+/ENmcVl68u/cMRl1JbW2bM+
/3A+SAg2c6iPDlehczKx2oa95QW0SkPPWGuNA/CE8CpyANIhu9XFrj3RQ3EqeRcS
AQQod1RNuHpfETLU/A2gMmvn/w/sx7TB3W5BPs6rprOA37tutPq9u6FTZOcG1Oqj
C/B7yTqgI7rbyvox7DEXoX7rIiEqyNNUguTk/u3SZ4VXE2kmxdmSh3TQvybfbnXV
4JbCZVaqiZraqc7oZMnRoWrXRG3ztbnbes/9qhRGI7PqXqeKJBztxRTEVj8ONs1d
WN5szTwaPIvhkhO3CO5ErU2rVdUr89wKpNXbBODFKRtgxUT70YpmJ46VVaqdAhOZ
D9EUUn4YaeLaS8AjSF/h7UkjOibNc4qVDiPP+rkehFWM66PVnP1Msh93tc+taIfC
EYVMxjh8zNbFuoc7fzvvrFILLe7ifvEIUqSVIC/AzplM/Jxw7buXFeGP1qVCBEHq
391d/9RAfaZ12zkwFsl+IKwE/OZxW8AHa9i1p4GO0YSNuczzEm4=
-----END CERTIFICATE-----
`

const maxPDF = 20 << 20 // счёт — десятки килобайт; предел — от чужого ответа на этом адресе

// Два клиента для PDF: с корнями сервера и с одним корнем Минцифры.
// Объединить их в один пул нельзя — системное хранилище плагину закрыто.
var pdfSystem, pdfMincifry *http.Client

// initPDF собирает клиентов один раз, при загрузке плагина: сертификат,
// который не разбирается, — ошибка загрузки, а не первого счёта.
func initPDF() error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(mincifryRootPEM)) {
		return errors.New("корневой сертификат Минцифры не разбирается")
	}
	pdfSystem = &http.Client{Timeout: 6 * time.Second, CheckRedirect: sameHost}
	pdfMincifry = &http.Client{Timeout: 6 * time.Second, CheckRedirect: sameHost,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			// Свой TLSClientConfig выключает HTTP/2, а Т-Банк такое
			// рукопожатие закрывает.
			ForceAttemptHTTP2: true,
		}}
	return nil
}

// sameHost — переходы при скачивании: не больше двух и только внутри того
// же хоста и той же схемы. Иначе ответ банка мог бы увести запрос магазина
// на произвольный адрес.
func sameHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 2 {
		return errors.New("слишком много переходов")
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return errors.New("переход на другой адрес")
	}
	return nil
}

// checkPDFURL — адрес PDF принимается только под префиксом из настроек
// (у банка — https://business.tbank.ru/invoices/): плагин не ходит по
// адресу, который назвал кто-то ещё. http разрешён, только если сам
// префикс — http: так работает тест с подделкой банка.
func checkPDFURL(raw, prefix string) error {
	u, err := url.Parse(raw)
	p, perr := url.Parse(prefix)
	if err != nil || perr != nil || !strings.HasPrefix(raw, prefix) || u.Host != p.Host || u.User != nil {
		return errors.New("адрес файла не от банка")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && p.Scheme == "http") {
		return errors.New("адрес файла не https")
	}
	return nil
}

// fetchPDF скачивает счёт: сначала с корнями сервера, а если цепочка
// сертификата им не знакома — с корнем Минцифры.
func fetchPDF(ctx context.Context, raw, prefix string) ([]byte, error) {
	if err := checkPDFURL(raw, prefix); err != nil {
		return nil, err
	}
	data, err := getPDF(ctx, pdfSystem, raw)
	// Ошибка приходит строкой из стандартной библиотеки: «x509: certificate
	// signed by unknown authority», «tls: failed to verify certificate».
	if err != nil && strings.Contains(err.Error(), "certificate") {
		data, err = getPDF(ctx, pdfMincifry, raw)
	}
	return data, err
}

func getPDF(ctx context.Context, client *http.Client, raw string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/pdf")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, errors.New("банк ответил кодом " + strconv.Itoa(res.StatusCode))
	}
	kind, _, _ := strings.Cut(res.Header.Get("Content-Type"), ";")
	if strings.ToLower(strings.TrimSpace(kind)) != "application/pdf" {
		return nil, errors.New("банк отдал не PDF")
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxPDF+1))
	if err != nil {
		return nil, err
	}
	// «%PDF-» — то же начало, которое проверяет ядро перед отдачей файла.
	if len(data) > maxPDF || !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, errors.New("банк отдал не PDF")
	}
	return data, nil
}
