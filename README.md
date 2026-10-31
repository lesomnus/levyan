# levyan

payday 앱의 tenant가 쓴 만큼 요금을 매겨 청구하는 서비스다. 미터, 가격, 청구서, 결제를 다루며, 그 자신이 자기 DB를 가진 [payday](https://github.com/lesomnus/payday) 앱이다. 처음 과금할 제품은 [loft](https://github.com/lesomnus/loft)다.

왜 이렇게 생겼는지는 [docs/design.md](docs/design.md)에 있다. 목표와 결정은 levyan#1, 구현 계획은 그 하위 이슈에 있다.

## 지금 있는 것

- **가격표.** 제품, 미터 정의, 가격 버전. 가격은 고치지 않고 새 버전을 낸다. 어느 기간에 어느 버전이 적용되는지 답한다. 보존 기간 미터는 구간마다 요금을 둔다.
- **호출자.** 제품은 서비스 키로 부르고 자기 tenant만 본다. 운영자는 따로 된 바이너리로 들어온다.
- **금액 계산.** 통화의 최소 단위 정수로, 무료량을 빼고, 끝에서 한 번 내림한다. 통화가 다른 금액은 더하지 않는다.
- **계량.** 제품이 보낸 보유량(읽은 값)과 흐름(OpenTelemetry 누적 Sum)을 바뀌지 않는 원장에 남긴다. 같은 값이 두 번 와도 한 행이고, 같은 시각에 다른 값이 오면 거부하고 로그에 남긴다. 이미 닫힌 기간에 대한 늦은 값도 받아서 정정 대상으로 표시한다. 와야 할 보유량이 오지 않으면 경보를 낸다.
- **금액 계산과 기간 닫기.** 기간은 제품의 zone에서의 달력 월이다. 기간이 끝나고 유예(48시간)가 지나면 닫고, 워크스페이스마다 내역(Statement)을 쓴다. 내역은 바꾸지 않는다. 늦은 값은 다음 내역의 정정 줄이 된다. 제품은 지금 기간의 사용량과 예상 금액, 그리고 내역을 읽는다.
- **권한 부여.** 워크스페이스마다 Grant(키, 값, 유효 기간)를 주고, payday의 `GrantService`로 제품에게 스트림과 목록을 준다. 제품은 자기 DB에 사본을 두고 검사한다. 운영자는 손으로 주고 끝낸다. 끝이 없는 Grant는 7일 앞까지 유효하고 루프가 갱신한다.

결제 계정은 다음 하위 이슈(#4)에서 온다. 그때까지 내역은 제품의 기본 채널과 통화로 매긴다. 앱이 할 일은 [docs/apps.md](docs/apps.md)에 있다.

## 두 바이너리

| | 무엇 | 어디서 |
| --- | --- | --- |
| `levyan` | 제품이 부르는 공개 바이너리. `serve`, `migrate` | `server.addr` (기본 `:50051`, gRPC) |
| `levyan-admin` | 운영자의 문(`serve`)과, 그 문에 묻는 명령들 | `admin.addr` (기본 `127.0.0.1:8081`, HTTP) |

`levyan`은 운영자 쪽 패키지를 링크하지 않고, `levyan-admin`도 공개 바이너리의 문을 링크하지 않는다. `go test ./cmd/levyan`이 `go list -deps`로 이를 확인한다. `admin.addr`는 운영자의 네트워크에만 연다.

## 실행

Go 1.27이 필요하다. 개발할 때는 SQLite 파일(`levyan.db`)을 쓴다.

```sh
go run ./cmd/levyan migrate             # DB를 이 빌드의 모양으로
go run ./cmd/levyan config env          # 설정할 수 있는 환경 변수 전부
```

운영자 키를 만들고, 키 파일을 가리키게 한다. 키는 32자 이상이고, 설정에는 키 자체가 아니라 `${file:…}`나 `${env:…}` 참조를 쓴다.

```sh
openssl rand -hex 32 > ~/.config/levyan-dev.key
export LEVYAN_ADMIN_KEYS="dev=\${file:$HOME/.config/levyan-dev.key}"

go run ./cmd/levyan-admin serve &       # 운영자의 문, 127.0.0.1:8081
go run ./cmd/levyan serve &             # 제품이 부르는 곳, :50051
```

### 가격표 다루기

운영자의 명령은 설정을 읽지 않는다. 문의 주소(`--url`, `LEVYAN_ADMIN_URL`)와 자기 키(`--key`, `LEVYAN_ADMIN_KEY`)만 있으면 된다. 바꾸는 명령은 `--yes`를 받아야 한다. 플래그는 인자보다 앞에 둔다.

```sh
export LEVYAN_ADMIN_KEY="\${file:$HOME/.config/levyan-dev.key}"
admin() { go run ./cmd/levyan-admin "$@"; }   # bash에서도 zsh에서도

admin product add --name Loft --yes loft
admin key issue --alias loft-dev --yes loft      # 키는 이때 한 번만 나온다

admin meter add --unit '{asset}' --kind stock --aggregation time_weighted_average \
  --name '활성 자산' --yes loft loft.assets.active
admin meter add --unit d --kind stock --aggregation last --unbounded-zero \
  --name '이력 보존 기간' --yes loft loft.history.keep

# 무료 100개, 그 뒤로 개당 50원. 2027-01-01부터.
admin price add --channel pg_kr --currency KRW --included 100 --unit-price 50 \
  --effective 2027-01-01 --desc '처음 가격' --yes loft loft.assets.active

# 인상: 새 고객은 3월부터, 기존 고객은 5월부터(두 달 고지).
admin price add --channel pg_kr --currency KRW --included 100 --unit-price 70 \
  --effective 2027-03-01 --existing 2027-05-01 --desc '인상' --yes loft loft.assets.active

# 보존 기간: 1년까지 0원, 3년까지 10,000원, 그 위(영원히 포함) 30,000원.
admin price add --channel pg_kr --currency KRW \
  --band 365:0 --band 1095:10000 --band :30000 --effective 2027-01-01 --yes loft loft.history.keep

admin product show loft
admin price ls loft
admin price timeline --channel pg_kr --currency KRW --since 2027-01-15 \
  --from 2027-01-15 --to 2027-07-01 loft loft.assets.active
```

가격 버전은 고칠 수 없다. 바꾸려면 새 버전을 낸다. 효력일은 지금보다 이를 수 없다.

### 제품이 부르는 API

제품은 `levyan.v1`을 부른다. `.proto`는 [api/levyan/v1](api/levyan/v1)에 있고, Go 앱은 그 모듈을 쓴다. 이 모듈은 gRPC와 protobuf만 요구하므로, 앱이 levyan의 payday 버전에 묶이지 않는다.

```sh
go get github.com/lesomnus/levyan/api@<commit>
```

```go
conn, err := grpc.NewClient(addr, creds)
books := levyanv1.NewPriceBookServiceClient(conn)
ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key)
ms, err := books.ListMeters(ctx, &levyanv1.ListMetersRequest{})
```

curl로 보려면 HTTP 리스너를 연다(`server.http.addr`, `server.http.allow_web: true`). 같은 서버, 같은 interceptor, 같은 wall이다.

```sh
curl -sX POST http://localhost:8080/levyan.v1.PriceBookService/ListMeters \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -H "Authorization: Bearer $KEY" -d '{}'
```

### 계량: 보유량과 흐름

제품이 보내는 것은 두 가지다. 어느 쪽이든 제품의 서비스 키로 부르고, 자기 미터와 자기 워크스페이스에 대해서만 말할 수 있다.

**보유량**(자산 수, 저장량, 보존 기간)은 `levyan.v1.MeteringService/Record`로 보낸다. payday의 `meter.Reader`가 잰 값(`meter.Reading`)을 그대로 옮긴 것이고, 워크스페이스는 제품 쪽 payday tenant id의 정식 텍스트다.

```sh
curl -sX POST http://localhost:8080/levyan.v1.MeteringService/Record \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -H "Authorization: Bearer $KEY" \
  -d '{"readings":[{"workspace":"<워크스페이스의 tenant id>","meter":"loft.assets.active",
       "measuredAt":"2027-01-15T00:00:00Z","value":120,"unit":"{asset}"}]}'
```

답은 보낸 값마다 하나다. `OUTCOME_KEPT`는 새로 남긴 것, `OUTCOME_ALREADY`는 이미 같은 값이 있던 것, `OUTCOME_REFUSED`는 남기지 않은 것이다(이유는 `refusal`과 `why`). 거부는 값 하나씩이라 나머지는 남는다. 호출이 실패하면 아무것도 남지 않았으므로 그대로 다시 보내면 된다. `late`는 이미 닫힌 기간에 대한 값이라 다음 내역에서 정정된다는 뜻이다.

**흐름**(전송량, 호출 수)은 OpenTelemetry 누적 Sum으로, 같은 주소(`server.addr`)에 OTLP/gRPC로 보낸다. payday의 `docs/guide/meters.md` 7장 그대로다. 모니터링 파이프라인이 아니라 청구용 `MeterProvider`를 따로 둔다.

```go
exp, err := otlpmetricgrpc.New(ctx,
	otlpmetricgrpc.WithEndpoint("levyan:50051"),
	otlpmetricgrpc.WithHeaders(map[string]string{"authorization": "Bearer " + key}),
)
billing := sdkmetric.NewMeterProvider(
	sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
	sdkmetric.WithResource(res), // service.name, 프로세스마다 service.instance.id
)
sent, err := billing.Meter("loft").Int64Counter("loft.transfer.bytes", metric.WithUnit("By"))
sent.Add(ctx, n, metric.WithAttributes(attribute.String(meter.TenantAttribute, tenant.String())))
```

받는 것은 미터 키로 이름 붙은, 단조 증가하는 누적 Sum의 정수 점뿐이다. 점마다 `payday.tenant`와 시작 시각이 있어야 하고, 단위는 미터의 것이어야 한다. 그 밖의 점은 하나씩 거부하고 OTLP의 partial success로 몇 개를 왜 거부했는지 알린다. 같은 점이 두 번 와도 한 번 센다.

**빠진 값.** `levyan serve`의 루프(`levyan.readings.gaps`)가 매시간, 최근 7일 안에 보유량을 보냈지만 36시간 넘게 보내지 않은 워크스페이스를 찾아 로그와 게이지 `levyan.readings.gaps`로 알린다. 기준은 `readings.gap`, `readings.recent`, `readings.every`로 바꾼다. 운영자는 바로 물을 수 있다.

```sh
admin gap ls                  # 모든 제품
admin gap ls --product loft
```

### 기간과 내역

제품의 조건을 정해야 기간을 닫는다. zone은 기간을 자르는 달력의 시간대이고, 채널과 통화는 결제 계정(#4)이 생기기 전까지 내역을 매기는 기준이다.

```sh
admin product configure --zone Asia/Seoul --channel pg_kr --currency KRW --yes loft
```

`levyan serve`의 루프(`levyan.periods.close`)가 끝난 지 `periods.grace`(기본 48시간)가 지난 기간을 닫는다. 운영자는 유예를 기다리지 않고 손으로 닫을 수 있다. 닫기를 두 번 해도 한 번 한 것과 같다.

```sh
admin period ls loft
admin period close --yes loft              # 지금까지 끝난 기간 모두
admin statement ls --workspace <id> loft   # 최근 기간부터
admin statement show <statement id>        # 줄마다 정확한 분수, 합계는 한 번 내린 값
```

내역에는 미터마다 줄이 있다. 줄에는 사용량, 무료량, 청구량, 가격 버전, 금액이 들어 있다. 이미 닫힌 기간에 대해 늦게 온 값이 있으면, 그 기간을 지금의 원장으로 다시 계산한다. 이미 청구한 것과 다르면 그 차이가 다음 내역의 정정 줄이 되고, 그 줄은 정정하는 기간을 가리킨다.

제품은 `levyan.v1.BillingService`로 읽는다.

```sh
curl -sX POST http://localhost:8080/levyan.v1.BillingService/GetUsage \
  -H 'Content-Type: application/json' -H 'Connect-Protocol-Version: 1' \
  -H "Authorization: Bearer $KEY" -d '{"workspace":"<워크스페이스의 tenant id>"}'
```

`GetUsage`는 지금 기간을 지금 닫는다면 내역이 무엇일지를 답한다. 각 미터의 최신 값이 기간 끝까지 이어진다고 보고 계산한 예상 금액이며, 닫기와 같은 계산이다. `ListStatements`와 `GetStatement`는 닫은 기간의 내역이다.

### 권한 부여

운영자는 워크스페이스에 손으로 Grant를 준다. 파일럿 고객과 계약 고객에게 쓴다. 워크스페이스는 제품 쪽 payday tenant id다. `--until`이 없으면 7일(`grants.horizon`) 앞까지 유효하고, `levyan serve`의 루프(`levyan.grants.renew`)가 반이 남을 때마다 갱신한다. trail에는 그 운영자가 남는다.

```sh
admin grant add --workspace <id> --key loft.export --on --source trial --desc '파일럿' --yes loft
admin grant add --workspace <id> --key loft.seats --limit 20 --until 2027-06-30 --source contract --yes loft
admin grant add --workspace <id> --key loft.seats --limit 30 --replace --source contract --yes loft  # 앞의 것을 이것의 시작에서 끝낸다
admin grant ls --workspace <id> loft
admin grant end --yes <grant id>
```

값은 `--on`, `--limit N`, `--unlimited` 중 하나다. 한도는 더할 양이 아니라 전체다. 빼앗는 것은 끝내는 것이다. 끝난 Grant는 48시간 동안 제품의 목록에 남고, 그 뒤에는 목록에서 빠진다(`grant ls --all`로 본다). 규칙이 준 Grant는 규칙의 것이라 운영자가 끝내지 않는다.

제품은 payday의 `payday.GrantService`(`Watch`, `List`)를 같은 주소에서 같은 키로 부른다. payday의 `entitle.Follower`가 그것으로 사본을 둔다([docs/apps.md](docs/apps.md)). levyan은 Grant의 변경을 늘 outbox에 쓰고, drain 루프(`payday.outbox`)가 broker로 낸다. 그래서 `watch.broker: none`으로는 시작하지 않는다.

## 배포

이미지는 `ghcr.io/lesomnus/levyan:edge`이고, `main`에서만 올라간다. 두 바이너리가 다 들어 있다. 기본 엔트리포인트는 `levyan serve`이고, 운영자의 문은 `--entrypoint /usr/local/bin/levyan-admin`으로 띄운다.

운영 DB는 PostgreSQL이다.

```sh
LEVYAN_DB_DRIVER=pgx LEVYAN_DB_DSN='postgres://…' levyan migrate
```

여러 프로세스로 띄우면 `spin.lease: postgres`와 `watch.broker: postgres`를 쓴다. 이미지 안에는 설정 파일이 없다. 모든 것은 `LEVYAN_*` 환경 변수나 `--config`로 받는다. 비밀(운영자 키, trail의 키)은 파일로 마운트하고 `${file:…}`로 가리킨다. 로그에는 남지 않는다.

## 개발

```sh
scripts/gen.sh                # 스키마(proto/)나 API(api/)를 고친 뒤: pd gen, 그리고 API
go tool pd gen --check .      # CI가 돌리는 것: 생성 결과가 스키마와 같은지
scripts/api.sh --check        # 그 다음에: API의 생성 코드가 .proto와 같은지
go tool pd doctor .

go test ./...                                             # SQLite
PDTEST_POSTGRES='postgres://…?sslmode=disable' go test ./... # PostgreSQL
```

`pd gen`은 자기가 고정한 buf를 받아서 쓰고, `scripts/api.sh`도 그 buf를 쓴다. API(`api/`)는 따로 된 모듈이라 `pd gen`이 건드리지 않는다(payday#189). `scripts/gen.sh`가 둘을 차례로 한다. 이 저장소에는 페이지(TS)가 없으므로 node는 필요 없다.

`CLAUDE.md`는 이 저장소에서 일하는 법을 영어로 적은 것이다.
