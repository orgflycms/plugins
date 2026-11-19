package tbank

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"strings"
	"time"
)

// mincifryRootPEM — корневой сертификат НУЦ Минцифры (Russian Trusted Root
// CA, действует до 27.02.2032, SHA-256 D2:6D:2D:02:…:CA:8E:CF:31).
// securepay.tinkoff.ru отдаёт цепочку этого корня, а в обычном
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

// fallbackRootPEM — корневой сертификат запасного клиента; по умолчанию —
// НУЦ Минцифры, в тестах подменяется через SetFallbackRoot до вызова Init.
var fallbackRootPEM = mincifryRootPEM

// SetFallbackRoot подменяет корневой сертификат запасного клиента (для тестов).
func SetFallbackRoot(pem string) { fallbackRootPEM = pem }

// Два клиента для API банка: с корнями сервера и с одним корнем Минцифры.
// Объединить их в один пул нельзя — системное хранилище плагину закрыто.
var bankSystem, bankMincifry *http.Client

// initBankClients собирает клиентов один раз, при загрузке плагина:
// сертификат, который не разбирается, — ошибка загрузки, а не первой оплаты.
func initBankClients() error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(fallbackRootPEM)) {
		return errors.New("корневой сертификат Минцифры не разбирается")
	}
	bankSystem = &http.Client{Timeout: 8 * time.Second, CheckRedirect: sameHost}
	bankMincifry = &http.Client{
		Timeout:       8 * time.Second,
		CheckRedirect: sameHost,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			// Свой TLSClientConfig выключает HTTP/2, а Т-Банк такое
			// рукопожатие закрывает.
			ForceAttemptHTTP2: true,
		},
	}
	return nil
}

// sameHost — переходы при запросе к банку: не больше двух и только внутри
// того же хоста и той же схемы. Иначе ответ банка мог бы увести запрос
// магазина на произвольный адрес.
func sameHost(req *http.Request, via []*http.Request) error {
	if len(via) >= 2 {
		return errors.New("слишком много переходов")
	}
	if req.URL.Scheme != via[0].URL.Scheme || req.URL.Host != via[0].URL.Host {
		return errors.New("переход на другой адрес")
	}
	return nil
}

// postBank отправляет POST JSON в банк: сначала с корнями сервера, а при
// ошибке проверки цепочки («unknown authority») повторяет тот же запрос
// клиентом с корнем Минцифры. При провале TLS запрос до банка не дошёл,
// поэтому повтор безопасен.
func postBank(ctx context.Context, endpoint string, data []byte) (*http.Response, error) {
	res, err := postJSON(ctx, bankSystem, endpoint, data)
	if err != nil && isUnknownAuthority(err) {
		res, err = postJSON(ctx, bankMincifry, endpoint, data)
	}
	return res, err
}

func postJSON(ctx context.Context, client *http.Client, endpoint string, data []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return client.Do(req)
}

func isUnknownAuthority(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "unknown authority") || strings.Contains(s, "system roots")
}
