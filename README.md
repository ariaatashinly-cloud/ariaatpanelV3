# ariaatashin 3.0 🔥

پنل فارسی مدیریت اتصال با رابط آتشی، مناسب موبایل، روی هستهٔ رسمی **3x-ui 3.8.5 / Xray**. نام قبلی اشتباهی atiaatashin بود؛ این نسخه نام درست ariaatashin را دارد و دادهٔ قدیمی را می‌خواند.

پس از ورود مدیر، دامنهٔ همین مرورگر تشخیص داده می‌شود؛ Address، Host، SNI، TLS، پورت عمومی و اشتراک از آن ساخته می‌شوند. انتخاب پروفایل Deplexo، Velixir یا Pxxl در «میزبان و سرور مجازی» راهنمای مناسب را نشان می‌دهد. تغییر پروفایل، خودِ برنامه را بین سرویس‌ها منتقل نمی‌کند.

نسخهٔ ۳، مدیریت اتصال و فروشگاه را در یک پنل جمع می‌کند: سفارش و تحویل خودکار، صفحهٔ مشتری، کیف پول، تمدید، کد تخفیف، تیکت، ربات فروش و مدیریت، API Key و اتصال به ariaatashin / 3x-ui 3.8.5 / Marzban کلاسیک. تصویر شعلهٔ طبیعی و طراحی فارسی واکنش‌گرا اضافه شده‌اند.

بازیابی نشست داخلی هسته، بررسی تازه پیش از ساخت کاربر و راه‌اندازی دوبارهٔ هسته پس از خروج پیاده‌سازی شده‌اند. ذخیرهٔ پایدار همچنان به Volume واقعی میزبان و یک Replica وابسته است.

- [ارتقا از نصب فعلی](UPGRADE-FA.md)
- [راه‌اندازی فروشگاه](SALES-FA.md)
- [ربات مدیریت و فروش](TELEGRAM-FA.md)
- [اتصال ربات نمونه و API](API-FA.md)
- [گزارش آزمون نسخهٔ ۳](TEST-REPORT-v3.md)


## قابلیت‌ها

- کاربران تکی یا گروهی تا ۵۰ نفر در هر درخواست، سهمیهٔ مشترک، اعتبار، خاموش/روشن، تعویض کلید، ریست مصرف، QR و Subscription.
- چهار الگوی میزبان وب و ده الگوی مستقیم VPS؛ فقط الگوهای قابل اجرا قابل انتخاب‌اند.
- نمودار واقعی شبکه و نشانگرهای منابع سیستم، فیلتر الگوها، رابط RTL و ناوبری موبایل.
- ربات تلگرام خصوصی با /status، /users، /sub و اعلان اختیاری؛ توکن در API و بکاپ نمی‌آید.
- کلید REALITY، Path، UUID و رمزها خودکار تولید می‌شوند و با Volume واقعی حفظ می‌شوند.
- خروجی لینک Xray و قطعهٔ outbound برای sing-box؛ XHTTP فقط در خروجی Xray می‌آید.
- نصب VPS با Docker Compose و Caddy برای TLS عمومی، نگه‌داری داده و گواهی در Volume و ری‌استارت پس از تمدید گواهی.

| حالت | الگوها |
|---|---|
| Cloud | VLESS + WS، VLESS + XHTTP، VMess + WS، Trojan + WS |
| VPS + REALITY | VLESS TCP/Vision، VLESS XHTTP |
| VPS + TLS | VLESS TCP/Vision، Trojan TCP، VMess TCP، VLESS gRPC، Trojan gRPC |
| VPS + TCP/UDP | Shadowsocks chacha20-ietf-poly1305 |
| VPS + UDP/TLS | TUIC v5، Hysteria2 |

WireGuard، AmneziaWG، چندمدیری و نمایندگی در این نسخه پیاده‌سازی نشده‌اند. فروش با رسید و تأیید مدیر، کیف پول و پرداخت اختیاری Telegram Stars اضافه شده است؛ اتصال درگاه بانکی نیازمند backend تأیید پرداخت خودت است. TUIC توسط sidecar واقعی همراه هسته اجرا می‌شود؛ Hysteria2 توسط Xray.

## شروع

- میزبان وب و آپلود در GitHub: **INSTALL-FA.md**
- سرور مجازی: **INSTALL-VPS-FA.md**
- تلگرام: **TELEGRAM-FA.md**
- آزمون‌ها و محدودیت‌ها: **TEST-REPORT.md**

برای میزبان وب، Dockerfile ریشه، Linux amd64، PORT=8080، یک Replica و ARIA_ADMIN_PASSWORD خصوصی حداقل ۱۲ کاراکتر لازم است. متغیر دامنه اجباری نیست. همان پوشهٔ web را کامل نگه دار.

### به‌روزرسانی از نسخهٔ قبلی

ابتدا بکاپ خصوصی بگیر؛ فایل‌های کد را جایگزین کن و **همان پروژه، Volume، پوشهٔ داده و رمز** را حفظ کن. ATIA_* همچنان پشتیبانی می‌شود؛ ARIA_* اولویت دارد. نام فایل قدیمی atia-settings.json و کلید Volume قدیمی atia-data عمداً برای سازگاری حفظ شده‌اند. حذف Volume باعث پاک‌شدن داده می‌شود.

به‌روزرسانی خودکار و بررسی‌نشدهٔ هسته انجام نمی‌شود. موتور فعلی با checksum ثابت بسته‌بندی شده؛ برای نسخه‌های بعدی هسته باید سازگاری API، قالب پروتکل‌ها و آزمون‌های واقعی دوباره بررسی شوند.

## سازگاری میزبانی

Pxxl طبق اسناد رسمی منابع GitHub/Docker و WebSocket دارد، بنابراین در یک پلن Docker مناسب قابل استفاده است؛ دیپلوی در حساب واقعی آن انجام نشده. امکانات پلن، خروجی اینترنت و filesystem Volume باید بررسی شوند. فضای Object Storage جانشین SQLite روی Volume نیست.

Deplexo ریشهٔ فقط‌خواندنی، /tmp موقت noexec و /data پایدار دارد؛ باینری‌ها در image باقی می‌مانند و تنظیمات در /data نوشته می‌شوند. Velixir دیسک محلی موقت دارد، حتی در پلن پولی؛ بکاپ مستقل لازم است. هیچ انتخابی در پنل، فضای موقت میزبان را دائمی نمی‌کند.

## منابع اصلی

- [3x-ui 3.8.5](https://github.com/MHSanaei/3x-ui/tree/v3.8.5)
- [Pxxl deployment](https://docs.pxxl.app/deploy/sources) · [WebSocket](https://docs.pxxl.app/troubleshooting/proxy-security) · [Storage](https://docs.pxxl.app/storage/overview)
- [Deplexo storage](https://docs.deplexo.com/operations/storage/) · [configuration](https://docs.deplexo.com/reference/configuration/)
- [Velixir limits](https://velixir.net/docs/limits)
- [TUIC spec](https://github.com/tuic-protocol/tuic/blob/master/SPEC.md)
- [Hysteria2 URI](https://v2.hysteria.network/docs/developers/URI-Scheme/)
- [Telegram Bot API](https://core.telegram.org/bots/api)
- [Caddy reverse proxy](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)

این پروژه وابسته یا مورد تأیید این سرویس‌ها نیست. شرایط استفاده و امکانات شبکهٔ هر میزبان همچنان اعمال می‌شود.
