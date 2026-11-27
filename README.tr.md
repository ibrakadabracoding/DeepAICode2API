<div align="center">

<img src="docs/assets/logo.png" alt="DeepAICode2API" width="128" height="128">

# DeepAICode2API

Yerel **DeepAI Code masaüstü uygulamasının** arkasındaki upstream model servisini tersine
mühendislikle yeniden üreten, hem **OpenAI hem de Anthropic** protokolünü konuşan bir **Go
gateway**'idir.

Masaüstü uygulamanın giriş yaptıktan sonra yazdığı API anahtarını yeniden kullanır; ayrı bir
anahtar başvurusu gerekmez. Saf Go standard kütüphanesiyle yazılmıştır, tek bir ~10 MB binary
olarak gelir ve dış bağımlılığı yoktur.

**[中文](README.md) / [English](README.en.md) / Türkçe**

[![ci](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml/badge.svg)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml) [![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](https://go.dev) [![deps](https://img.shields.io/badge/deps-0%20(stdlib)-00ADD8)](go.mod) [![tests](https://img.shields.io/badge/tests-653%20cases-brightgreen)](https://github.com/2951461586/DeepAICode2API/actions/workflows/ci.yml)
[![protocol](https://img.shields.io/badge/protocol-OpenAI%20%7C%20Anthropic-6b4fbb)](#3-uç-noktalar-endpointler) [![docker](https://img.shields.io/badge/docker-ready-2496ED?logo=docker&logoColor=white)](docker-compose.yml) [![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Ask DeepWiki](https://img.shields.io/badge/Ask%20DeepWiki-blue.svg?labelColor=000000&logo=data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAACAAAAAgCAYAAABzenr0AAAACXBIWXMAAAPoAAAD6AG1e1JrAAACIUlEQVRYw+2XP2gUQRTGv7d3JhYWQawkIBaCVWzETos0gmAnVoKNTSoLK0E7EQQrC20sBVFBtLPQRgTBtBaKjYgIQUSxiJq7fT+LvCGPJZdNLt4dQj5Y3u7sznzfzLw/s9IO/icANiniCujEfWdsQgBLxAbsbraPZcmBfcAt4DNwKQsZNfEUsAB8YRV12LfA6ZHuedg7AO7ed/c+UBcbQhbiu+6wXIM6EnZWkszMJe2Ke0n6I2la0v51hFeSMLN6OwIK6jJwEBdUYb2MAxRSz6sY4geiahFgLe1Hwq6YWe3us8AN4FQQU4QM64SPY69Xyr43fADgNjAPXHb3peSsD4FDQ0VLEjAHvIhBPS6AHvA1bBO9JPAXcHFbIRtJ5yzwIQZ9CZwAusDJENIkLqsG8DpNyIZJwSUk9wLHgakcesC1JCCjiHm1kYDNOEjptCxpSVK/8f73OFLxPLAYM3oCzEX7MXf/lJbc/8kWpA4HgQdpYI9IWAbeh83whi84cHXL+58EPBoQhnmm94EzwE3gZ2p/Dhzdbhg+TaRr01x7ftb4/jBwFzjXrCvDpmLW9crVLNeR9CaapoGemb2TdCERW1tNaBNQ8jktwmozqwtpqRNtdWAjARYk39IS12ZWRX7vmJmA71nQZgi36gN7gCvAj5xc3P0jcH7kx7Ik5IC73wsh14GZsZySow5002l4Jr0b6+m4eSyvpAn8lEzsx2QHo8RfUrlN+uPq4ksAAAAASUVORK5CYII=)](https://deepwiki.com/2951461586/DeepAICode2API)

</div>

> Bu dosya, bu repoyu kendi ortamımda hızlıca anlamak ve çalıştırmak için tuttuğum Türkçe
> referans çevirisidir. Orijinal/upstream dokümanlar: [README.md](README.md) (Çince) ve
> [README.en.md](README.en.md) (İngilizce). Makineye özel kısa operasyon notları için
> [README.local.md](README.local.md) dosyasına bakın.

| Yetenek | Ayrıntı |
| --- | --- |
| Çift protokol | OpenAI `/v1/chat/completions`, `/v1/responses`; Anthropic `/v1/messages` |
| Protokol uyumu | Lane 8 + `X-DeepAI-Code-Instance` + `/theos` check-in; `social_login_required` / `machine_required` / `lane_version_unsupported` ayrımı |
| Hesap havuzu yönetimi | Grup round-robin, failover, kapasite backoff'u, hesap başına kota zamanlaması, politika engelli hesapların otomatik atlanması |
| Hesap ekleme | Tarayıcı ile giriş (Google / Apple / e-posta+şifre), masaüstü kimlik bilgisi içe aktarma, manuel API key; havuza girişte otomatik makine parmak izi ve proxy atanır |
| Konsol | Tek gömülü sayfa: hesaplar / anahtarlar / loglar / istatistikler / ayarlar |
| Güvenlik | Anahtar doğrulama, yönetici parolası, same-origin yazma koruması, isteğe bağlı AES-256-GCM durum dosyası şifreleme |
| Regresyon paketi | 616 vaka (468 üst seviye + 148 alt test) / 14.7k satır test; `go test -race` + gofmt / vet / build / vulncheck tamamen yeşil; CI testli 10 paketi (5 pakette test yok) + frontend sözdizimi ve smoke kontrollerini (23) kapsar |

```text
┌──────────────┐  /v1/chat/completions   ┌────────────────┐   Api-Key + Theos header'ları ┌─────────────────────────────┐
│ OpenAI SDK   │  /v1/responses          │ DeepAICode2API │  X-DeepAI-Code-Machine        │ https://api.deepai.org      │
│ Claude Code  │  /v1/messages           │   (gateway)    │ ────────────────────────────▶ │ /deepai_code/chat/completions│
│ pi / Cline   │  /v1/models             │                │ ◀──────────────────────────── │ /deepai_code/responses      │
└──────────────┘  SSE akış / akışsız     └───────┬────────┘      OpenAI uyumlu SSE         └─────────────────────────────┘
                                                  │ otomatik okur
                                                  ▼
                          ~/.deepai-code/userdata/secrets/deepai-account.bin
                          {"apiKey":"…","username":"…","isPro":false}
```

---

## İçindekiler

- [1. Bu nedir?](#1-bu-nedir)
- [2. Hızlı başlangıç](#2-hızlı-başlangıç)
- [3. Uç noktalar (endpoint'ler)](#3-uç-noktalar-endpointler)
- [4. İstemci yapılandırması](#4-i̇stemci-yapılandırması)
- [5. Yapılandırma başvurusu](#5-yapılandırma-başvurusu)
- [6. Test ve doğrulama](#6-test-ve-doğrulama)
- [7. Güvenlik sınırı](#7-güvenlik-sınırı)
- [8. Mimari ve dizin yapısı](#8-mimari-ve-dizin-yapısı)
- [9. Dağıtım (Docker)](#9-dağıtım-docker)
- [10. Bilinen tuzaklar ve sınırlar](#10-bilinen-tuzaklar-ve-sınırlar)
- [11. Doküman indeksi](#11-doküman-indeksi)
- [12. Lisans](#12-lisans)

---

## 1. Bu nedir?

**DeepAI Code** (masaüstü uygulaması sürüm 0.3.52, kontrol tarihi 2026-11-25), OpenAI
uyumlu `/chat/completions` ve `/responses` uç noktalarını açan `api.deepai.org`
üzerindeki `deepai_code/*` servislerini çağırır; kimlik doğrulama için `Api-Key` header'ı
kullanılır. Bu gateway, bu yolu yeniden üretir ve üç protokol ağzını tek bir sürece taşır:

- OpenAI Chat Completions (`/v1/chat/completions`)
- OpenAI Responses API (`/v1/responses`)
- Anthropic Messages (`/v1/messages`)

Aynı zamanda çoklu hesap havuzu, kota algılama, tarayıcı ile hesap ekleme ve gömülü bir
yönetim konsolu sağlar.

### 1.1 Tersine mühendislik özeti

| Aşama | Tersine mühendislik sonucu |
| --- | --- |
| Uygulama biçimi | Electron kabuğu (`app.asar`) + ayrı server (`server.asar`, ~200 MB) |
| Veri dizini | `T3CODE_HOME` veya `~/.deepai-code`, durum dizini `<base>/userdata` |
| **Kimlik bilgisi diske yazımı** | `<stateDir>/secrets/deepai-account.bin`, **düz metin JSON** (dosya izni 0600) |
| Kimlik bilgisi içeriği | `{apiKey, username, isPro}`, `apiKey` UUID biçimindedir |
| **Çıkarım girişi** | `POST https://api.deepai.org/deepai_code/chat/completions` |
| Responses girişi | `POST https://api.deepai.org/deepai_code/responses` (yalnızca `api=openai-responses` modelleri) |
| Model kataloğu | `GET https://api.deepai.org/deepai_code/models` |
| Kota | `GET https://api.deepai.org/deepai_code/theos` |
| Kullanım | `GET https://api.deepai.org/get_usage_summary` |
| Ses transkripsiyonu | `POST https://api.deepai.org/speech_to_text` (`voiceRecording` alanı) |
| Web araması | `POST https://api.deepai.org/deepai_code/web_search` |
| **Kimlik doğrulama** | İstek header'ı `Api-Key: <apiKey>` (`Authorization` değil) |
| Protokol header'ı | `X-DeepAI-Code-Theos: 8` (faturalama lane sürümü; 0.3.41–0.3.50 için 7, **0.3.52'de 8'e çıktı**). **Lane eşiği sunucu davranışıdır**: 2026-11-18 (0.3.41 yayın günü) yalnızca 7 geçiyordu; 2026-11-19'da ≤7 hepsi geçer, 8+ 426 oldu; **2026-11-25 kontrolünde pencere biçimine döndü: 7 / 8 geçer, 6 ve 9 reddedilir** (`docs/recon.md` §3) |
| Self-proof header'ı | `X-DeepAI-Code-Proof` (0.3.52'de eklendi, lane 8): değer, paketle gelen native helper `resource-monitor --attest` tarafından build anahtarıyla hesaplanır; `/theos` ve model çağrıları sırasıyla "check-in kanıtı" ve "istek etiketi" taşır. **Gateway bunu uygulamaz**: 2026-11-25 canlı testinde upstream doğrulamıyordu — eksik veya uydurma hex de 200 döndü |
| Süreç örneği | `X-DeepAI-Code-Instance: <32 hex>` (her çıkış proxy'si için bir kez rastgele, o proxy'nin tüm JSON isteklerinde taşınır; masaüstünde süreç başına bir kez) |
| Check-in | `/theos` yanıtı `check_in:{nonce, seconds}` taşır; bir sonraki `/theos` çağrısı `X-DeepAI-Code-Check-In` ve `X-DeepAI-Code-Beacon: v=…;up=…;pi=0|1` geri gönderir (`focus` yalnızca odak durumu biliniyorsa eklenir) |
| **Makine parmak izi** | `X-DeepAI-Code-Machine: sha256(ham makine UUID'si)`, günlük ücretsiz Theos tahsisi için kullanılır; ölçülen modellerde eksikse `machine_required` döner |
| Oturum yapışkanlığı | `X-Session-Affinity` (body içindeki `prompt_cache_key` de upstream tarafından kullanılır); gateway bunu ayrıca belirli bir hesaba bağlar, böylece aynı oturumun prefix cache'i aynı hesapta kalır (bkz. §10) |
| Kapasite reddi | `409 {"error":{"code":"session_busy"}}` + `Retry-After` |
| Hesap politikası | `403 social_login_required`: 0.3.19 aşamalı daraltması, 2026-11-09 tekrar testinde kaldırıldı; gateway yine de `blocked` terminal durumu olarak işler (bkz. `docs/recon.md` §4.1) |
| Tarayıcı girişi | `GET /deepai_code/desktop_auth/?state=…&port=…&code_challenge=…&code_challenge_method=S256` → yerel `/auth-callback?code=…` → `POST /deepai_code/desktop_auth/exchange/` (body'de `code_verifier`). PKCE 0.3.45 masaüstü ile uyumludur, `DEEPAI2API_LOGIN_PKCE=0` ile eski biçime dönülebilir |

### 1.2 Upstream'in kendisi de OpenAI uyumludur

`chat/completions` istek/yanıtı OpenAI ile tamamen uyumludur; yanıtta ayrıca
OpenRouter tarzı `provider`, `reasoning`, `reasoning_details`, `cost` gibi alanlar taşınır
(`id: gen-…`, `usage.cost_details.upstream_inference_*`, `native_finish_reason`).
`provider` alanının **gerçek çıkarım backend adını** (`InferenceNet` / `Wafer` / `Reka` / ...)
taşıdığına dikkat edin; **yedek (substitute) dal artık `provider` alanını atlar** (eski
`provider: "DeepAI"` gitti). Gerçek/yedek ayrımı için `provider` eksik mi, `model` istenen
`z-ai/<id>`'yi yankılıyor mu ve `id` öneki `gen-` mi `gen_` mi birlikte kontrol edilmelidir
(tam kriter tablosu için `docs/recon.md` §3.1).

Bu nedenle gateway karmaşık protokol restorasyonuna ihtiyaç duymaz; odak şunlardır:

1. **Kimlik bilgisi içe aktarma**: masaüstü düz metin kimlik dosyasından okuyup tekilleştirme;
2. **Hesap havuzu**: çoklu anahtar rotasyonu, başka hesaba failover, `session_busy`'de bekle-yeniden dene;
3. **Model yönetişimi**: dinamik katalog önbelleği, alias'lar, fallback modeller;
4. **Protokol adaptasyonu**: Anthropic ve Responses istemcileri için çeviri;
5. **Gözlemlenebilirlik**: istek logları, token/maliyet istatistikleri, kota probu, konsol.

### 1.3 Makine parmak izi nasıl türetilir

Masaüstü mantığı (`src/deepai/machineId.ts`):

- Windows: `(Get-CimInstance Win32_ComputerSystemProduct).UUID`; firmware UUID'si placeholder ise
  registry `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`'e düşer;
- macOS: `ioreg` içinden `IOPlatformUUID`;
- Linux: `/sys/class/dmi/id/product_uuid`, sonra `/etc/machine-id`;
- Son olarak `sha256(lowercased raw value)` alınır; ham değer hiçbir zaman gönderilmez.

Gateway **ham UUID'yi saklar ve çalışma anında hash'ler**; böylece state dosyasını bir makineden
diğerine kopyalamak fingerprint'i geçersiz kılmaz. İlk çalıştırmada `DEEPAI_MACHINE_ID` (ham
UUID, zorunlu değil) okunur; yoksa rastgele bir UUID üretilip state'e yazılır. Masaüstünün gerçek
fingerprint'i ile aynı gün ücretsiz Theos kotasını paylaşmak isterseniz aynı ham makine UUID'sini
verin.

> Mevcut bir state dosyasında eski `machine_id_hash` (64 hex) alanı varsa gateway bunu boş ham
> UUID olarak kabul eder ve uyarı loglar.

---

## 2. Hızlı başlangıç

### 2.1 Ön koşullar

- Go **1.26+** (veya doğrudan [Release](https://github.com/2951461586/DeepAICode2API/releases) binary'si indirin)
- En az bir DeepAI Code hesabı (masaüstünde giriş yapılmış **veya** tarayıcıdan eklenecek)

### 2.2 Derle ve çalıştır

```bash
git clone https://github.com/2951461586/DeepAICode2API.git
cd DeepAICode2API
make build
./bin/deepaicode2api
# Varsayılan dinleme: http://127.0.0.1:10087
# Konsol:            http://127.0.0.1:10087/
# Loglar stderr'e gider
```

Arka planda çalıştırmak isterseniz: `nohup ./bin/deepaicode2api > deepaicode2api.log 2>&1 &`

İlk çalıştırmada gateway şunları yapar:

1. Masaüstünün kimlik dosyasını tarar ve kullanılabilir hesapları havuza aktarır
   (devre dışı bırakmak için `DEEPAI2API_IMPORT_ON_STARTUP=0`; yeniden etkinleştirmeden önce
   mevcut içe aktarılan kayıtları temizleyin, aksi halde dedup nedeniyle tekrar alınmazlar);
2. Her yeni hesaba rastgele bir makine parmak izi atar ve havuzdaki proxy'leri round-robin
   bağlar (proxy yoksa doğrudan bağlantı);
3. Hiç gateway anahtarı yoksa `sk-...` üretir ve başlangıçta bir kez loglar; anahtarı konsolun
   **Keys** sekmesinden de kopyalayabilirsiniz.

Dışarı açmanız gerekiyorsa `-host 0.0.0.0` (veya `DEEPAI2API_HOST`) geçin; **kesinlikle önce
`DEEPAI2API_ADMIN_PASSWORD` ayarlayın** (aksi halde `/` ve `/admin/*` herkese açık olur; §7'ye bakın).

### 2.3 Docker Compose ile çalıştırma (önerilir)

```bash
cp .env.example .env
# .env içine en azından şunları yazın:
#   DEEPAI2API_ADMIN_PASSWORD=<rastgele güçlü parola>
#   DEEPAI_API_KEY=<manuel ekleyecekseniz; masaüstü aktarımı/tarayıcı girişi kullanacaksanız boş bırakın>

docker compose up -d --build
curl http://127.0.0.1:10087/healthz
```

`docker-compose.yml` varsayılan olarak `10087:10087` yayınlar, state dosyasını
`deepaicode2api-data` named volume'una bağlar; konteyner içinde varsayılan yol
`/data/deepaicode2api-data.json`'dur.

**İsteğe bağlı env değişkenleri** (`.env.example` içinde açıklamalı tam liste):

| Değişken | Amaç |
| --- | --- |
| `DEEPAI_API_KEY` | Manuel tek/çoklu hesap (`key1,key2` veya satır satır); masaüstü aktarımı/tarayıcı girişi kullanıyorsanız boş bırakın |
| `DEEPAI_BASE_URL` | Özel upstream adresi (reverse proxy / ayna) |
| `DEEPAI_PROXY` / `DEEPAI_PROXIES` | Çıkış proxy havuzu; her hesaba round-robin bağlanır |
| `DEEPAI2API_ADDR` | Dışarı verilen portu değiştir (örn. `8080:10087`) |
| `DEEPAI2API_WEB_DIR` | Konsolu harici dizinle değiştir (hot-reload; gömülü olan varsayılan) |
| `DEEPAI2API_DATA_PATH` | State dosyası yolu (container default `/data/...`) |
| `DEEPAI2API_STATE_KEY` | State dosyası AES-256-GCM şifreleme anahtarı (en az 16 karakter; geçmiş loglar dahil tam şifreleme) |
| `DEEPAI2API_LOG_FULL_PAYLOAD` | Loglara tam request/response body yaz (`1` = tam, sınırsız; varsayılan kapalı = baş/son örnekleme). **Prompt'lar ve yanıtlar diske iner — sadece kendi ortamınızda açın** |

### 2.4 Doğrulama

```bash
# 1) Sağlık kontrolü (kimlik doğrulama yok)
curl http://127.0.0.1:10087/healthz

# 2) Model listesi (gateway anahtarı gerekir)
curl http://127.0.0.1:10087/v1/models \
  -H "Authorization: Bearer $DEEPAI2API_API_KEY"

# 3) OpenAI Chat Completions
curl http://127.0.0.1:10087/v1/chat/completions \
  -H "Authorization: Bearer $DEEPAI2API_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"merhaba"}]}'

# 4) Anthropic Messages (Claude Code ile aynı protokol)
curl http://127.0.0.1:10087/v1/messages \
  -H "x-api-key: $DEEPAI2API_API_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4.1-flash","max_tokens":64,"messages":[{"role":"user","content":"merhaba"}]}'
```

---

## 3. Uç noktalar (endpoint'ler)

### 3.1 OpenAI uyumlu

| Metot | Yol | Açıklama |
| --- | --- | --- |
| GET | `/healthz` | Liveness probe, kimlik doğrulama yok |
| GET | `/v1/models` | OpenAI model listesi |
| GET | `/v1/models/{id}` | Tek model detayı |
| POST | `/v1/chat/completions` | OpenAI Chat; `stream: true` SSE döner |
| POST | `/v1/responses` | OpenAI Responses API (`api=openai-responses` modelleri doğrudan upstream'e gider; diğerleri Chat üzerinden sentezlenir) |
| POST | `/v1/audio/transcriptions` | OpenAI uyumlu ses transkripsiyonu (`multipart/form-data`, alan adı `file`) |
| GET | `/v1/deepai/usage` | Upstream kullanım özeti (yönlendirme) |

### 3.2 Anthropic uyumlu

| Metot | Yol | Açıklama |
| --- | --- | --- |
| POST | `/v1/messages` | Anthropic Messages API; `stream: true` SSE döner |
| POST | `/v1/messages/count_tokens` | Yerel yaklaşık token sayımı (200 döner, upstream çağrılmaz) |

### 3.3 Gateway uzantıları

| Metot | Yol | Açıklama |
| --- | --- | --- |
| GET | `/api/quota` | Havuzdaki tüm hesapların Theos kotasını sorgular |
| GET | `/api/quota?email=…` | Tek hesabın kotasını sorgular |
| POST | `/v1/deepai/search` | Upstream web araması yönlendirmesi |
| POST | `/v1/deepai/speech_to_text` | Doğrudan upstream transkripsiyon yönlendirmesi (ham alan adı `voiceRecording`) |

> `/v1/chat/completions` bir **geçirgen (passthrough) katmanıdır**: bilinmeyen alanlar olduğu
> gibi upstream'e iletilir, böylece `tools` / `tool_choice` / `response_format` /
> `reasoning_effort` / `thinking` ve yeni upstream parametreleri ekstra eşleme olmadan çalışır.
> Yalnızca model yönlendirmesi ve `stream` normalizasyonu yapılır.

### 3.4 Kimlik doğrulama ve yetki

- **Gateway API anahtarı** (`DEEPAI2API_API_KEY` veya konsoldan üretilen `sk-...`):
  `Authorization: Bearer <key>` / `X-Api-Key` / `?api_key=` ile `/v1/*` ve `/api/*`
  çağrılarında kullanılır. `/api/quota` da aynı anahtarı ister.
- **Yönetici parolası** (`DEEPAI2API_ADMIN_PASSWORD`): konsol girişi ve tüm `/admin/*`
  yazma işlemleri için. Yazma isteklerinde ayrıca aynı origin (veya `Sec-Fetch-Site: same-origin`)
  gerekir. Aynı origin kanıtı olmadan cross-origin yazma 403 döner.
- Anahtar yoksa `/v1/*` anonim erişime açıktır (sadece yerel kullanım içindir).

---

## 4. İstemci yapılandırması

### 4.1 OpenAI uyumlu istemciler (Cherry Studio / Open WebUI / SDK)

```text
Base URL: http://127.0.0.1:10087/v1
API Key:  <gateway anahtarınız, sk-...>
Model:    deepseek-v4.1-flash   (veya /v1/models listesinden bir id)
```

### 4.2 Claude Code (Anthropic protokolü)

`~/.claude/settings.json`:

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://127.0.0.1:10087",
    "ANTHROPIC_AUTH_TOKEN": "sk-your-gateway-key"
  }
}
```

> Model kimlikleri DeepAI tarafındaki orijinal id'lerdir (örn. `deepseek-v4.1-flash`). Bir istemci sabit bir
> model id'si gönderiyorsa, gateway bunu yalnızca `DEEPAI_MODEL_MAP` içinde açık bir alias varsa çözer;
> bilinmeyen bir id sessizce başka bir modele yönlendirilmez (yanlış faturalamayı önlemek için).

### 4.3 Pi / Cline / OpenCode

OpenAI uyumlu provider olarak `baseURL=http://127.0.0.1:10087/v1` ile ekleyin.

---

## 5. Yapılandırma başvurusu

Tüm yapılandırma **flag > env > varsayılan** önceliğiyle okunur. `.env` dosyası başlangıçta
yüklenir (path: `DEEPAI2API_ENV_PATH`, varsayılan `.env`). Tam şablon için `.env.example` dosyasına bakın.

| Değişken / Flag | Varsayılan | Açıklama |
| --- | ---: | --- |
| `DEEPAI2API_HOST` / `-host` | `127.0.0.1` | Dinleme adresi |
| `DEEPAI2API_PORT` / `-port` | `10087` | Dinleme portu |
| `DEEPAI2API_ADMIN_PASSWORD` / `-admin-password` | boş | Konsol ve `/admin/*` parolası |
| `DEEPAI2API_ADMIN_COOKIE_SECURE` | auto | Admin session cookie'ye `Secure` bas; varsayılan: doğrudan HTTPS veya güvenilir proxy arkasında HTTPS'te açık |
| `DEEPAI2API_TRUST_PROXY_HEADERS` | `0` | `1` ise `X-Forwarded-Proto/Host` gibi header'lara güven (reverse proxy arkasında açın) |
| `DEEPAI2API_API_KEY` / `-api-key` | boş | Gateway erişim anahtarı (çoklu virgülle ayrılır) |
| `DEEPAI2API_DATA_PATH` / `-data` | OS user-state dizini + `/deepaicode2api-data.json` | State dosyası yolu |
| `DEEPAI2API_STATE_KEY` | boş | State dosyası AES-256-GCM anahtarı (en az 16 karakter) |
| `DEEPAI_API_KEY` / `-deepai-api-key` | boş | Upstream DeepAI API anahtarı (çoklu) |
| `DEEPAI_BASE_URL` / `-deepai-base-url` | `https://api.deepai.org` | Upstream base URL |
| `DEEPAI_MACHINE_ID` / `-deepai-machine-id` | rastgele UUID | Ham makine UUID'si (gateway hash'ler) |
| `DEEPAI_PROXY` / `-deepai-proxy` | boş | Varsayılan çıkış proxy'si |
| `DEEPAI_PROXIES` | boş | Proxy havuzu (virgül/satır ayrılmış) |
| `DEEPAI_PROXY_FILE` | boş | Satır başına bir proxy içeren dosya yolu |
| `DEEPAI2API_IMPORT_ON_STARTUP` | `1` | Başlangıçta masaüstü kimlik dosyasını tara/aktar |
| `DEEPAI2API_DESKTOP_DIR` | auto | Masaüstü state dizini override |
| `DEEPAI2API_MODEL` / `-model` | boş | Varsayılan model override |
| `DEEPAI_MODELS` / `-models` | boş | Katalog başarısız olduğunda fallback model listesi |
| `DEEPAI_MODEL_MAP` | boş | Model alias eşlemesi (`from:to,from2:to2`) |
| `DEEPAI2API_WEB_DIR` / `-web-dir` | gömülü | Konsolu harici dizinden servis et |
| `DEEPAI2API_ENV_PATH` / `-env` | `.env` | Yüklenecek .env dosyası |
| `DEEPAI2API_ADDR` | boş | Docker compose publish adresi (`8080:10087` gibi) |
| `DEEPAI2API_IMAGE` / `VERSION` | `latest` / `dev` | Docker imaj etiketi (deploy script) |

> Masaüstü kimlik dosyası içe aktarma, yerel makinede çalışan resmi DeepAI Code istemcisinin
> zaten yazdığı dosyayı okur; bu yol yeni kimlik bilgisi üretmez veya üçüncü taraf adına hesap
> oluşturmaz.

---

## 6. Test ve doğrulama

```bash
make test           # go test ./...
make test-race      # race detector ile
make vet            # go vet ./...
make lint           # gofmt kontrolü + vet
make cover          # coverage raporu
make build          # binary üret
```

Kabul tabanı (upstream README ile aynı iddia):

- 616 vaka (468 üst seviye + 148 alt test), 14.7k satır test
- `go test -race` + gofmt / vet / build / vulncheck yeşil
- CI 10 testli paketi kapsar (5 pakette test yok) + frontend syntax/smoke (23)

> Not: Bu ortamda Go toolchain veya bağımlılıklar farklı olabilir; doğrulamayı çalıştırmadan önce
> `go version` ve gerekiyorsa modül bağımlılıklarını kontrol edin.

---

## 7. Güvenlik sınırı

- **Varsayılan dinleme `127.0.0.1`'dir**. Dışarı açıyorsanız mutlaka yönetici parolası ayarlayın ve
  mümkünse TLS/ters proxy arkasına koyun.
- `DEEPAI2API_STATE_KEY` ayarlanırsa state dosyası AES-256-GCM ile şifrelenir (API anahtarları,
  hesaplar, loglar dahil). Anahtarı kaybederseniz state kurtarılamaz.
- Admin yazma işlemleri same-origin koruması taşır; tarayıcı dışı istemcilerde uygun header'ları
  gönderdiğinizden emin olun.
- Upstream DeepAI API anahtarları ve makine UUID'si hassastır; logları/ekran görüntülerini paylaşırken
  maskeleyin.
- Bu proje yerel olarak zaten giriş yapılmış istemcinin mevcut yolunu yeniden kullanır. DeepAI hizmet
  şartlarına uyum sizin sorumluluğunuzdadır.

---

## 8. Mimari ve dizin yapısı

```text
main.go                     wiring: flag → storage → route → signal (iş mantığı yok)
tasks.go                    arka plan görevleri: persist / katalog yenileme / kota guardian + import/seal komutları
env.go                      flag/env ayrıştırma (varsayılanlar merkezileşmiş; bkz. §5)
cmd/probe/                  tanılama aracı
internal/deepai/            upstream istemcisi (protokol, SSE, makine parmak izi, kimlik aktarımı)
internal/envfile/           .env yükleme (başlangıçta etkili)
internal/registry/          model kataloğu çözümleme ve alias'lar
internal/store/             state persist + hesap havuzu zamanlaması (dosya başına ≤400 satır hedefi)
  ├─ store_open.go          Open / Save / snapshot / atomik yazma
  ├─ store_accounts.go      hesap CRUD / sayfalama / zamanlama girişleri
  ├─ store_keys.go          gateway anahtarları
  ├─ store_models.go        model kataloğu / makine parmak izleri / ayarlar
  ├─ store_telemetry.go     istatistik ve log sorguları
  ├─ reseal.go              -seal: mevcut düz metni yeniden mühürle (geçmiş loglar dahil)
  └─ state_seal.go          AES-256-GCM envelope / anahtar parmak izi
internal/proxy/             proxy havuzu (round-robin / maskeli etiket / dosya yükleme)
internal/pool/              hesap seçim politikası (grup round-robin / kota kararı / cooldown); Store olmadan test edilebilir
internal/parallel/          sınırlı eşzamanlı fan-out (eşzamanlılık sınırı / ctx iptal / indeks sıralı yazma)
internal/retry/             backoff retry (eşikler / jitter / retry edilemez sınıflandırma)
internal/httpx/             saf HTTP yardımcıları ve middleware (JSON yanıt / hata zarfı / decode / gzip / ETag)
internal/gateway/           protokol çevirisi: alan eşleme + usage ayrıştırma; HTTP/store/pool bağımlılığı yok
  ├─ anthropic_types.go     Anthropic istek modeli (Messages / tools)
  ├─ anthropic_translate.go Anthropic ⇄ Chat çift yönlü alan eşleme
  ├─ anthropic_stream.go    akış content-block yöneticisi (thinking / text / tool_use)
  ├─ responses_translate.go Responses ⇄ Chat alan eşleme
  └─ usage.go               Usage + upstream usage ayrıştırma + toleranslı JSON erişicileri
internal/server/            HTTP route'ları, akış I/O, konsol (birleştirme katmanı)
  ├─ chat.go                OpenAI Chat Completions (passthrough + normalizasyon)
  ├─ anthropic.go           Anthropic Messages girişi
  ├─ responses.go           Responses API girişi (passthrough / Chat sentezi)
  ├─ responses_stream.go    Responses akış olay üreticisi (sseWriter yazar, bu pakette kalır)
  ├─ upstream.go            openModel: hesap seçimi / slot alma / failover / hata sınıflandırma
  ├─ accounts.go            hesap slotları, eşzamanlılık sınırları, kabul kuyruğu
  ├─ models.go              model kataloğu çözümleme ve alias fallback
  ├─ sse.go                 SSE yazımı ve [DONE]
  ├─ reqlog.go              istek başına enstrümantasyon ve istatistik persist
  ├─ probe.go               kota probları (istek üzerine / guardian / batch checkin)
  ├─ extensions.go          quota, usage, search, speech
  ├─ login.go               tarayıcı girişi (RFC 8252 loopback callback)
  ├─ admin_accounts.go      hesap admin girişi: GET sayfalama / POST action dispatch
  ├─ admin_actions.go       her biri minimal istek struct'ına sahip 10 action
  ├─ gateway_alias.go       gateway için kısa paket-içi adlar (type alias, geçiş uyumu)
  ├─ web.go                 konsol asset route whitelist'i (asset'ler web/ paketinde)
  └─ web/                   konsol statik paketi (assets.go: go:embed + ETag + CSP)
       ├─ index.html        sayfa markup'u (301 satır)
       ├─ style.css         stiller (design token + layout)
       ├─ js/app.js         birleştirme: veri çekme / URL state / başlangıç sırası (231 satır)
       ├─ js/events.js      olay bağlama: nav / üst bar / filtre / sıralama / dil / maskeleme
       ├─ js/theme.js       tema, sidebar daraltma, mobil drawer
       ├─ js/pager.js       hesap pager'ı (sayfa no / sayfa boyutu)
       ├─ js/i18n.js        sözlük + metin değiştirme (bağımlılık yok)
       ├─ js/format.js      saf biçimlendirme (bağımlılık yok)
       ├─ js/state.js       paylaşılan mutable UI state (sayfalama cursor'u)
       ├─ js/dom.js         DOM yardımcıları + satır menüsü
       ├─ js/table.js       header sıralama state'i ve saf fonksiyonlar
       ├─ js/api.js         admin API çağrıları (auth / hata normalizasyonu / dedup / timeout retry)
       ├─ js/ui.js          toast
       ├─ js/actions.js     hesap / anahtar mutasyonları + kota probları (reload setReload ile enjekte edilir)
       └─ js/render/        dört render modülü: accounts / keys / logs / settings
```

Katmanlama ilkeleri:

- **`deepai`** yalnızca upstream protokolünü bilir (path, header, hata semantiği, SSE); havuzun
  varlığından habersizdir.
- **`store`** yalnızca state ve zamanlamayı yönetir (persist, gruplar, failover, kota kararları);
  ağ isteği göndermez.
- **`server`** yalnızca protokol çevirisi ve auth yapar; hesap seçimi her zaman `Store` üzerinden
  gider.

Böylece upstream protokol değişiklikleri yalnızca `deepai`'yi, zamanlama politikası değişiklikleri
yalnızca `store`'u etkiler; testler sahte upstream ve geçici state dosyasıyla bağımsız çalışabilir.

---

## 9. Dağıtım (Docker)

```bash
docker build -t deepaicode2api .
docker run -d --name deepaicode2api \
  -p 10087:10087 \
  -e DEEPAI_API_KEY=<api-key> \
  -e DEEPAI_MACHINE_ID=<ham makine UUID'si> \
  -e DEEPAI2API_ADMIN_PASSWORD=<yönetici parolası> \
  -v $PWD/data:/data \
  -e DEEPAI2API_DATA_PATH=/data/deepaicode2api-data.json \
  deepaicode2api
```

**Uzun süreli dağıtımlar için `scripts/deploy.sh` tercih edin** (build → immutable tag → up → health bekle):

```bash
sudo install -m 600 /dev/null /etc/deepaicode2api.env   # sırlar (hesaplar / parolalar / proxy'ler / makine parmak izi)
sh scripts/deploy.sh v0.1.13                            # deploy; başarısız olursa log ve rollback komutu basar
DEEPAI2API_IMAGE=<eski tag> sh scripts/deploy.sh         # belirli bir imaja geri dön
```

- Sırları `DEEPAI2API_ENV_FILE` içinde tutun (varsayılan `/etc/deepaicode2api.env`, 0600), **kod
  dizininde değil**; `.env` yalnızca `VERSION` / `DEEPAI2API_IMAGE` / `DEEPAI2API_ADDR` gibi dağıtım
  parametrelerini tutsun.
- Container içinde `healthcheck` (imaj içi `wget /healthz`, 30 sn) ve `mem_limit: 512m` bulunur;
  bunlar olmadan `unless-stopped` kilitlenen süreci yeniden başlatmaz (2026-11-22 production
  retrospektifi).

> `X-DeepAI-Code-Theos: 8` mevcut istemcinin (0.3.52) lane'idir (0.3.41–0.3.50 boyunca 7). Lane eşiği upstream
> tarafından her an ayarlanabilir: 2026-11-18'de "yalnızca 7"; 2026-11-19'da "≤7 hepsi geçer, 8 ve
> üstü 426 `lane_version_unsupported` döner"e gevşedi (bkz. `docs/recon.md` §3). Eski lane göndermek
> **hala çalışabilir**, bu nedenle protokolün güncel olup olmadığı `/theos` `lane_version` değerine
> göre değerlendirilir (konsol özetindeki "upstream lane").
> `X-DeepAI-Code-Machine` ölçülü modeller için hala gereklidir: yoksa `400 machine_required` döner.
> Container'lar genelde host SMBIOS'unu okuyamaz; bu yüzden **dağıtımda mutlaka
> `DEEPAI_MACHINE_ID=<ham makine UUID'si>` geçin** (gateway kendi hash'ler). `DEEPAI_API_KEY` de
> verilmelidir; aksi halde gateway "kimlik yok" modunda başlar.

Aynı makineyi birden fazla ortamda çalıştırırken state/proxy/port çakışmalarından kaçının;
ayrıntılar ve geriye dönük uyumluluk notları için İngilizce/Çince README'deki ilgili bölüme bakın.

---

## 10. Bilinen tuzaklar ve sınırlar

| Belirti | Kök neden | İşlem / Önleme |
| --- | --- | --- |
| `/v1/messages` boş `content` döndürüyor | Upstream model yanıt vermedi; gateway eskiden `stop_reason=end_turn` ile sessizce 200 döndürüyordu | Artık `502 upstream_empty_response`; istemci retry etmeli |
| İstemci "Usage exhausted" görüyor ama kota bitmemiş | Upstream tekil hesabı geçici kısıtladı (hesap havuzu değil) | Gateway bunu kapasite sınırlaması olarak ele alır: mevcut hesapları dener, sonra 429 + `Retry-After` döner |
| `lane_version_unsupported` (426) | Upstream lane eşiğini değiştirdi; istemci lane'i reddedildi | Gateway'in lane/header güncellemesini alın; geçici olarak desteklenen lane'e sabitlemeyin, upstream pencere davranışı değişebiliyor |
| `machine_required` (400) | Ölçülü model isteğinde `X-DeepAI-Code-Machine` yok/yanlış | `DEEPAI_MACHINE_ID` geçin veya state'teki makine kimliğini düzeltin |
| `session_busy` (409) | Aynı upstream oturumu aynı anda meşgul | Gateway aynı hesabı kısa backoff ile yeniden dener, sonra başka hesaba geçer |
| Akış kesilmesi / yarım body | Upstream SSE bağlantısı koptu | İstemci tarafında yeniden deneme ve idempotent akış işleme uygulayın |
| 401/403 geri dönüyor | Gateway anahtarı eksik/yanlış veya hesap `blocked/deleted` | Konsoldan anahtarı ve hesap durumunu kontrol edin |
| `429 Too Many Requests` | Havuz kapasitesi/geçici kota baskısı | `Retry-After`'a uyun; hesap/proxy havuzunu büyütün |

Diğer önemli notlar:

- `/v1/chat/completions` geçirgendir; upstream'in yeni alanları çoğu zaman ek eşleme olmadan çalışır.
- Aynı oturum için `X-Session-Affinity`/`prompt_cache_key` mümkün olduğunca sabit tutulmalıdır; gateway bunu hesap yapışkanlığına da bağlar.
- `provider` alanı gerçek backend adını taşır; yedek dalda eksik olabilir — gerçek/yedek ayrımı için `provider`, `model` yankısı ve `id` öneklerini birlikte değerlendirin (bkz. `docs/recon.md` §3.1).

---

## 11. Doküman indeksi

**"Durum" alanına bakmadan içeriğe güvenmeyin**: `maintained` = mevcut kodla tutarlı, referans
alınabilir; `historical` = bağlam için tutulan süreç kaydı, sonuçları sonraki kontrollerde
değişmiş olabilir (en güncel kayıtlar için `docs/changes.md` ve `docs/recon.md`).

| Dosya | İçerik | Durum |
| --- | --- | --- |
| `README.md` | Genel bakış, uç noktalar, parametreler, kabul tabanı ve bilinen tuzaklar (Çince orijinal) | Maintained |
| `README.en.md` | Yukarıdakinin tam İngilizce çevirisi (2026-11-22 eklendi) | Maintained (Çince ile senkron) |
| `README.tr.md` | Bu dosya: Türkçe referans/çeviri | Maintained (kullanıcı referansı) |
| `README.local.md` | Bu makineye özel kısa operasyon notu (systemd, yollar, hızlı testler) | Yerel not |
| `docs/recon.md` | Tersine mühendislik hızlı referansı: upstream uç noktalar, header'lar, hata semantiği, repro komutları | Maintained (son kontrol 2026-11-22) |
| `docs/changes.md` | 2026-11 değişiklik günlüğü: kök nedenler, kararlar, kabul | Maintained |
| `docs/lessons.md` | **Mühendislik dersleri**: stres turları → yeniden kullanılabilir kurallar + kontrol listesi | Maintained |
| `docs/performance.md` | Stres verisi ve optimizasyon planı: tek hesap E2E, faz kırılımı, P0/P1/P2 kazanç tahminleri | Maintained (veri snapshot 2026-11-10) |
| `docs/pi-lens.md` | Bu repodaki pi-lens yapılandırması: ne kapalı, neden, nasıl doğrulanır/geri alınır | Maintained |
| `docs/model-fingerprint-2026-11-10.md` | Havuzdaki modellerin parmak izi ölçümleri (11-10 tam tarama + 11-11/11-24/11-25 takipleri) | Historical snapshot |
| `docs/optimization-plan.md` | İlk taramanın tam optimizasyon planı | Historical: uygulanmış |
| `docs/optimization-plan-r2.md` | İkinci tarama (write amplification / konsol / re-probe) planı ve kazanç tahminleri | Historical: uygulanmış |
| `docs/assets/` | Proje ikonları: `logo.png` / `logo.ico`; konsol `/favicon.ico` aynı dosyadır |
| `.env.example` | Açıklamalı tam yapılandırma şablonu |
| `Makefile` | `make build / test / test-race / lint / cover` |

## 12. Lisans

MIT. Bu proje yalnızca zaten giriş yapılmış yerel istemcinin çağrı yolunu yeniden üretir; lütfen
DeepAI hizmet şartlarına uyun.

> **İstemcinin kendi beyanı (0.3.43'te eklendi, 0.3.44 / 0.3.45 / 0.3.47 / 0.3.48 / 0.3.50 / 0.3.52'de hala mevcut; 0.3.52'de `--attest` helper'ını da kapsayacak şekilde genişletildi)**: `server.asar` içindeki
> `src/deepai/api.ts` dosyasında lane header'ı ve check-in yanında bir yorum DeepAI ToS §4'ü anar
> ve bu header'ların ve check-in'in "DeepAI Code app self-identification" için var olduğunu,
> **başka istemcilere kopyalanmaması gerektiğini** açıkça belirtir. Bu proje yalnızca zaten giriş
> yapılmış yerel istemcinin mevcut yolunu, kendi hesapları için yeniden kullanır ve başkası adına
> dağıtmaz/işletmez; bu header'ları başka yerde kullanmak istiyorsanız önce izin durumunu kendiniz
> doğrulayın.
