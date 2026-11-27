# DeepAICode2API — Bu Makine İçin Kısa Not

Bu dosya, bu sunucudaki kurulumu hızlıca ayağa kaldırmak / kontrol etmek için
tutulan yerel nottur. Ayrıntılı teknik doküman: [README.md](README.md).

## Ne çalışıyor?

- **Docker container yok.** Servis, `systemd --user` altında yerel binary olarak çalışıyor.
- Servis adı: `deepaicode2api.service`
- Adres: `http://127.0.0.1:10087` (yalnızca bu makinenin localhost'u)
- Binary: `/home/ubuntu/repos/DeepAICode2API/bin/deepaicode2api`
- Sürüm: `v0.1.29` (DeepAI Code istemci 0.3.52 / lane 8 ile uyumlu)
- Veri dosyası: `/home/ubuntu/.local/share/deepaicode2api/deepaicode2api-data.json`
- Kontrol paneli: <http://127.0.0.1:10087/>
- Upstream hesabı: `~/.deepai-code/userdata/secrets/deepai-account.bin` dosyasından
  otomatik içe aktarılır.

> 2026-11-26: Upstream istemci 0.3.52 / lane 8. Bu makinede `v0.1.29` release
> binary'si çalışıyor. Go kurulu olmadığı için kaynaktan derleme yerine resmi
> release binary'si kullanılıyor. Eski `v0.1.6` binary'si + state dosyası
> `~/.local/share/deepaicode2api/backup-20261126-v0.1.6/` altında duruyor.

## Başlat / durdur / yeniden başlat

```bash
systemctl --user start deepaicode2api
systemctl --user stop deepaicode2api
systemctl --user restart deepaicode2api
systemctl --user status deepaicode2api --no-pager
```

Canlı log:

```bash
journalctl --user -u deepaicode2api -f
```

Son 100 satır:

```bash
journalctl --user -u deepaicode2api -n 100 --no-pager
```

Makine yeniden başladığında servisin kendiliğinden açılması için:

```bash
sudo loginctl enable-linger "$USER"
systemctl --user enable deepaicode2api
```

Bu makinede linger zaten açık (`Linger=yes`).

## Çalışıyor mu? Hızlı test

```bash
# Panel ayakta mı?
curl -sS -o /dev/null -w '%{http_code}\n' http://127.0.0.1:10087/

# Gateway API anahtarı (OpenCode'un kullandığı anahtar)
KEY=$(cat ~/.config/opencode/deepaicode.key)

# Model listesi
curl -sS http://127.0.0.1:10087/v1/models \
  -H "Authorization: Bearer $KEY" | jq -r '.data[].id'

# DeepSeek ile gerçek bir cevap
curl -sS http://127.0.0.1:10087/v1/chat/completions \
  -H "Authorization: Bearer $KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"Merhaba"}],"max_tokens":64}' \
  | jq -r '.choices[0].message.content'
```

Beklenen: model listesinde `deepseek-v4.1-flash` görünür ve son komut bir metin döndürür.

## Context window: 320k yazısı ne anlama geliyor?

- Model kataloğu ve DeepAI Code arayüzü **320.000 token** gösterir; bu *nominal*
  değerdir, ham kullanılabilir token garantisi değildir.
- Upstream'in gerçek darboğazı **istek gövdesi ~196 KB**. Bu sınır aşıldığında
  upstream hata döndürmez; sessizce baştaki/system mesajlarını atar ve
  `usage.prompt_tokens` küçülür (issue #7).
- 2026-11-26 testi: 194 KB gövde → tam işlendi (28.769 prompt token; baştaki ve
  ortadaki kod hatırlandı). 196 KB gövde → ~1.5k token'a düştü; kodlar kayboldu.
- Bu yüzden pratikte İngilizce ~40k token (dile göre ~40-60k) güvenli. 320k
  token'a ulaşmak için ~1.3 MB gövde gerekir; upstream bunu ham olarak işlemiyor,
  ~196 KB'de kırpıyor.
- DeepAI Code uygulaması 320k'yı bir **istemci sıkıştırma bütçesi** olarak
  kullanıyor olabilir (uzun konuşmayı özetleyip gönderiyor). Bu, ham 320k token'ın
  upstream'e gittiği anlamına gelmez.
- Gateway'in `/v1/chat/completions` yolu şeffaf proxy'dir; otomatik sıkıştırma
  yok. `/v1/models` içindeki `context_body_bytes: 196608` gerçek sınırı gösterir.
  OpenCode'da `limit.context: 1048576` bırakmak yerine ~40k'ya çekmek veya
  gateway'de `body_cap_mode=reject` kullanmak daha güvenli.
- `body_cap_mode=reject` ile birlikte `context_body_bytes` değerini **190000**
  civarına çekin: varsayılan `196608`, gözlenen ~195.8 KB kırılmasının üstünde
  kalır; 196 KB'lık bir gövde yine sessizce kırpılabilir.

## Reasoning effort: max vs high

- 2026-11-26 testi (24-game, aynı prompt): ortalama reasoning token
  `low 146 / medium 154 / high 175 / xhigh 188 / max 194`.
- `high` vs `max` 7 eşleşmiş koşu: max 5/7 daha fazla, 1 eşit, 1 az;
  ortalama **194 vs 175** (~%11).
- Sonuç: `max` ignore edilmiyor; kalabilir. Daha hızlı cevap için `high` yeterli.

## İstemci ayarı

OpenAI uyumlu herhangi bir istemci için:

- Base URL: `http://127.0.0.1:10087/v1`
- API key: `~/.config/opencode/deepaicode.key` içindeki `sk-...` değeri
- Model: `deepseek-v4.1-flash` (ücretsiz)

OpenCode tarafındaki ayar zaten `~/.config/opencode/opencode.jsonc` içinde:

```jsonc
"baseURL": "http://127.0.0.1:10087/v1",
"apiKey": "{file:~/.config/opencode/deepaicode.key}"
```

Servis yalnızca localhost'a bağlı. İstemci başka bir makinedeyse SSH tüneli kullan:

```bash
ssh -L 10087:127.0.0.1:10087 ubuntu@<bu-sunucu>
```

Sonra istemciyi yine `http://127.0.0.1:10087/v1` adresine yönlendir.

## Güncelleme

Upstream istemci sürümü değiştiğinde (özellikle `426 lane_version_unsupported`
hatası alındığında) en yeni release'i indirip binary'yi değiştir. Bu makinede Go
kurulu değil; `make build` yerine aşağıdaki release binary yöntemini kullan:

```bash
cd /home/ubuntu/repos/DeepAICode2API

TAG=$(curl -sS https://api.github.com/repos/2951461586/DeepAICode2API/releases/latest | jq -r .tag_name)
ARCH=arm64   # bu makine aarch64; x86_64 ise amd64 yaz

curl -fL -o /tmp/deepaicode2api \
  "https://github.com/2951461586/DeepAICode2API/releases/download/${TAG}/deepaicode2api_${TAG}_linux_${ARCH}"

install -m 0755 /tmp/deepaicode2api bin/deepaicode2api
systemctl --user restart deepaicode2api
journalctl --user -u deepaicode2api -n 30 --no-pager
```

Go kuruluysa kaynaktan da derlenebilir:

```bash
git pull --ff-only
make build
systemctl --user restart deepaicode2api
```

## Yedek / geri dönüş

Eski sürümlerin binary + veri yedekleri:

```text
~/.local/share/deepaicode2api/backup-20261116-v0.1.2/
~/.local/share/deepaicode2api/backup-20261126-v0.1.6/
```

`v0.1.29` kurulumundan önce `v0.1.6` binary'si ve state dosyası
`backup-20261126-v0.1.6/` altına alındı.

Geri dönmek gerekirse (önce servisi durdur). Not: `v0.1.2` / `v0.1.6` eski
lane'lere ait olduğu için güncel upstream'de `426 lane_version_unsupported`
dönebilir; bu yedekler daha çok veri/state kurtarma içindir:

```bash
systemctl --user stop deepaicode2api
cp ~/.local/share/deepaicode2api/backup-20261126-v0.1.6/deepaicode2api-v0.1.6 \
   ~/repos/DeepAICode2API/bin/deepaicode2api
systemctl --user start deepaicode2api
```

## Sık karşılaşılan hatalar

| Hata | Anlamı / çözüm |
| --- | --- |
| `426 lane_version_unsupported` | Upstream istemci sürümünü yükseltti. Yukarıdaki **Güncelleme** adımını yap. |
| `401` / `缺少 API 密钥` | Gateway API anahtarı eksik. `~/.config/opencode/deepaicode.key` dosyasını veya paneldeki **Keys** bölümünü kontrol et. |
| `403 theos_exhausted` | DeepAI hesabının günlük ücretsiz kotası bitmiş. Ücretli model kullanma veya kotanın yenilenmesini bekle. |
| `409 session_busy` | Upstream aynı anda meşgul; gateway otomatik yeniden dener. |
| Servis açılmıyor, port dolu | `ss -ltnp \| grep 10087` ile eski süreci bul; `systemctl --user status deepaicode2api` çıktısına bak. |
| Panel açılıyor ama model cevapsız | `journalctl --user -u deepaicode2api -n 100 --no-pager` çıktısında upstream hatasını ara. |
| Model eski mesajları unutuyor / logda `body_bytes` > 196 KB | Upstream istek gövdesini sessizce kırpıyor (issue #7). Konuşmayı sıkıştır veya gateway'de `body_cap_mode=reject` kullan. |

## Notlar

- Durum dosyası upstream API anahtarını içerir; izinleri `0600`. Yedeklerini
  paylaşma, git'e ekleme.
- İstersen durum dosyasını AES-256-GCM ile şifreleyebilirsin; ayrıntısı
  [README.md](README.md) §4 ve §5'te.
- Docker ile çalıştırmak zorunlu değil. Docker'a geçersen container içinde
  `DEEPAI_MACHINE_ID` ve `DEEPAI2API_ADMIN_PASSWORD` verilmesi gerekir; mevcut
  systemd kurulumu bu makinede daha basit ve çalışır durumda.
