# levyan 설계

levyan(levy + an)은 payday 앱의 tenant가 쓴 만큼 요금을 매겨 청구하는 서비스다. Lago 같은 청구 엔진을 payday로 직접 만들고, roster처럼 자기 DB를 가진 별도의 payday 앱으로 둔다. 처음 과금할 제품은 loft다.

이 문서는 levyan#1(목표, 정한 것, 설계)을 줄여 옮기고, #2·#3·#5·#6·#7과 #10·#12의 일부(PG 어댑터와 MoR 어댑터의 라이브러리)를 만들면서 정한 것을 더한 것이다. 누가 정했는지를 나눠 적는다.

- **대표**: 대표가 정한 것. levyan#1의 "정한 것 (2026-10-31, 대표)" 표뿐이다(2장).
- **Claude**: 계획을 맡아 진행하는 Claude(리드)가 대표의 "남은 작업 모두 진행해줘"를 맡아 정한 것. 대표의 결정이 아니며, 대표가 뒤집을 수 있다.
- **구현에서 정한 것**: 그 하위 이슈를 구현한 에이전트가 고른 것. 역시 대표가 뒤집을 수 있다.

## 1. 목표

- payday 앱의 tenant가 가진 양과 쓴 양을 재고, 가격표대로 금액을 계산해 결제 채널로 청구한다.
- 앱은 요금제 이름을 모른다. 앱은 권한(기능, 숫자 한도)만 검사하고, 그 권한은 levyan이 준다.
- 여러 SaaS 제품이 levyan 하나를 쓴다. MoR 계정, PG 계약, 웹훅, 대조를 제품마다 따로 두지 않는다.
- 돈이 걸린 정합성을 처음부터 시험으로 지킨다. 중복 청구가 없어야 하고, 빠진 청구가 없어야 하며, 틀린 청구는 정정할 수 있어야 한다.

## 2. 정한 것 (대표)

2026-10-31, levyan#1:

| 항목 | 결정 |
| --- | --- |
| 구현 | 직접 만든다. Lago·OpenMeter·Kill Bill·Flexprice는 개념만 참고한다 |
| 형태 | payday로 만든 별도 서비스(자기 DB). 앱 쪽에 필요한 것은 payday 확장으로 만든다 |
| 판매 채널 | 기본은 MoR, 국내 고객은 국내 PG. 사업이 커지면 감당할 수 있는 지역부터 그 지역의 PG를 늘린다 |
| 첫 제품 | loft |
| 과금 단위 | payday tenant(loft의 워크스페이스). 계량과 가격의 단위다 |
| 무엇으로 | 자산 수, 저장량 같은 보유량. 무료량을 둔다(값은 나중에 정한다) |
| 이력 | 용량이 아니라 보존 기간으로 과금한다 |
| 지불자 | 워크스페이스마다 지불자는 한 명. 무료 워크스페이스 수는 지불자 기준으로 센다 |
| 계정 | 계정 자체는 roster의 것. 계정을 가리키는 방식과 계정으로서 하는 호출은 payday가 정의한다 |
| 저장소 | 공개. 라이선스는 아직 넣지 않는다 |

### Lago를 쓰지 않는 이유 (levyan#1)

- 한국에서 결제가 안 된다. Lago가 기본으로 연동하는 Stripe에 한국 사업자는 가입할 수 없고, MoR을 쓰면 MoR과 Lago가 같은 구독을 함께 갖는다.
- 무료판의 제약(청구서·메일의 Lago 표시, Premium 기능)이 많다.
- Rails·Sidekiq·Redis·PDF 스택을 따로 운영해야 하고, payday의 trail·접속기록·개인정보 처리 밖에 놓인다.
- 돈이 걸린 경로의 공개 이슈(중복 청구, 멈춘 청구서 등)가 답 없이 열려 있다. 같은 종류의 문제를 payday는 구조로 푼다. 같은 트랜잭션의 outbox와 trail, 작업 ID 멱등, lease를 잡고 도는 sweep이다.
- AGPL-3.0이라 코드는 옮기지 않는다.

## 3. Claude가 정한 것 (진행을 맡기며, 대표의 결정 아님)

2026-10-31, #2·#3을 맡기며:

- **levyan의 tenant는 제품이다.** loft가 첫 tenant다. 제품의 호출은 그 제품의 tenant 안에서 이뤄지므로, 제품끼리는 wall이 갈라놓는다.
- **지불자 수준의 객체는 global이다.** 결제 계정 같은 것은 roster 계정이 갖는다. 다음 하위 이슈(#4)에서 만들되, 지금 자리를 남겨 둔다.
- **운영자는 따로 된 바이너리를 쓴다.** loft의 `loft-admin`처럼 자기 바이너리, 자기 주소, 운영자 키를 갖는다. 공개 바이너리는 운영자 패키지를 링크하지 않는다(`go list -deps`로 확인).
- **앱은 levyan의 payday 생성 코드를 링크하지 않는다.** loft가 levyan의 payday 버전에 묶이면 안 되기 때문이다. 어떻게 할지는 8장.

## 4. 책임과 경계

결제, 청구, 계량, 권한, 인가는 서로 다른 책임이다.

```text
   loft (payday app, own DB)
     - authorization, entitlement copy, limit checks   <- payday extension
     - meter readings, sent                            <- payday extension (payday#186)
     - tenant lifecycle events, sent
       |                         ^
       | readings, lifecycle     | grant changes
       v                         |
   levyan (payday app, own DB)
     - price book, billing accounts, links
     - meter ledger, rating, statements, invoices
     - payment ledger, webhook inbox, reconciliation
     - channel adapters: MoR | KR PG | regional PG (later)
       |
       v
   MoR / PG (money movement)
```

payday의 Event·Fact처럼 payday 안에 넣지 않고 별도 서비스로 둔 이유:

- **기록의 주인이 tenant가 아니다.** 청구서와 결제 기록은 tenant가 파기된 뒤에도 남아야 한다(세법상 장부와 증빙은 5년).
- **payday는 프레임워크다.** 청구를 고칠 때마다 payday 릴리스를 거치지 않는다.
- **결제 키를 앱 밖에 둔다.** 결제와 환불을 할 수 있는 키를 사용자가 닿는 앱의 프로세스와 DB에 두지 않는다.
- **제품이 늘어도 한 벌이다.**

| 데이터 | 원천 | 사본 |
| --- | --- | --- |
| 보유량·사용량 | 앱(다시 잴 수 있다) | levyan의 계량 원장: 그때 잰 값, 청구의 근거 |
| 가격표·가격 버전 | levyan | — |
| 결제 계정과 tenant 연결 | levyan | — |
| 구독 | PG 채널은 levyan, MoR 채널은 MoR | MoR 채널에서는 levyan이 사본을 가진다 |
| 금액 계산 내역 | levyan(모든 채널) | — |
| 법적 청구서·영수증 | PG 채널은 levyan, MoR 채널은 MoR | — |
| 결제(돈의 이동) | PG·MoR | levyan의 결제 원장. 대조로 맞춘다 |
| 권한 부여(Grant) | levyan | 앱의 권한 사본 |

## 5. 호출자와 tenancy

### 제품 (구현됨, #2)

- 제품은 levyan의 tenant(payday `Tenant`)다. 운영자가 세운다.
- 제품은 **서비스 키**로 부른다: `authorization: Bearer lvk_…`. 키는 제품 tenant의 행(`ServiceKey`)이고, 저장하는 것은 키의 SHA-256뿐이다. 키는 발급할 때 한 번만 보여 준다.
- 호출의 actor는 **키 자신**이고 tenant는 그 제품이다. 그래서 trail에는 어느 키가 무엇을 썼는지가 남고, 키를 폐기하는 것은 그 행을 지우는 것이다(soft erase). roster의 deployment 키와 같은 모양이다.
- 무엇을 부를 수 있는지는 키가 아니라 policy 표(`server/policy`)가 제품이라는 호출자 종류에 대해 정한다. 표에 없는 메서드는 거부된다. 스키마가 생성한 메서드는 누구의 몫도 아니다. 제품이 부르는 것은 `levyan.v1`(8장)과 payday의 `GrantService`(13장)다.
- policy는 제품에게 자기 tenant만 보여 준다(`Where`). 다른 제품의 행은 없는 것과 같다(NotFound).

### 운영자 (구현됨, #2)

- 운영자의 문은 `levyan-admin serve`다. 자기 바이너리, `admin.addr`(기본 loopback), HTTP JSON. 공개 바이너리는 이 패키지(`server/admin`, `cli/admin`)를 링크하지 않고, 운영자 바이너리도 공개 바이너리의 문(`cli`, `server/api`, `server/policy`)을 링크하지 않는다.
- 운영자 키는 설정(`admin.keys`)에 이름과 함께 비밀로 둔다(`${file:…}`, `${env:…}`, 32자 이상). 로그에는 이름만 남고 키는 남지 않는다.
- 운영자의 쓰기는 wall이 없는 서버(`Ungated`)로 간다. 제품을 세우는 것은 tenant 안에서 하는 일이 아니기 때문이다. 대신 frame에 운영자의 행을 actor로 넣는다(구현에서 정한 것). 그래서 trail이 "가격 버전을 누가 냈는가"에 답한다. 운영자의 행(`Operator`, global)은 `levyan-admin serve`가 시작할 때 `admin.keys`에 맞춘다.

### 지불자 (자리만 남김, #4)

- 결제 계정은 roster 계정이 갖는 global 행이 된다.
- 지불자의 호출은 tenant 없이 계정으로서 이뤄진다. 이 방식은 payday가 정한다(payday#184). 그때 policy 표에 지불자 열이 생긴다. 지금은 키가 아닌 frame은 아무것도 부를 수 없다.
- 지불자가 워크스페이스 하나를 볼 때는 payday의 field 3(tenant보다 작은 집합)을 쓸 수 있다. 제품 tenant 안의 워크스페이스가 그 집합이다. #4에서 정한다.

## 6. 가격표 (구현됨, #3)

무엇을 얼마에 파는지를 코드가 아니라 데이터로 둔다. 그래야 가격과 무료량을 나중에 정해도 엔진은 바뀌지 않는다.

### 미터 (`Meter`)

- 키(`loft.assets.active`), 단위(OpenTelemetry가 쓰는 UCUM: `By`, `{asset}`, `d`), 종류(보유량 stock, 흐름 flow), 기간 집계 방식을 갖는다.
- 집계: 보유량은 시간 가중 평균, 최대, 마지막 값 중 하나이고, 흐름은 합계다. 보유량의 합은 얼마나 자주 쟀는지에 달려 있고, 흐름의 최대는 아무도 재지 않은 비율이라서 그 조합은 거부한다(구현에서 정한 것).
- 키와 단위의 문법은 payday의 미터 선언과 같다(`meter.CheckKey`, `meter.CheckUnit`). 앱이 선언한 미터의 키가 그대로 levyan의 미터 키다.
- 키, 단위, 종류, 집계는 바뀌지 않는다. 다른 것을 세게 된 미터는 새 키다. 그리고 미터는 지우지 않는다. 지우면 키가 다시 비고, 다시 쓰인 키는 같은 이름으로 다른 것을 청구하게 된다(구현에서 정한 것).
- `unbounded_zero`: 값 0이 "한없음"이라는 뜻이다. loft의 `keep_days` 0은 영원히 보존이다. 보유량만 그렇게 말할 수 있다.

### 가격 버전 (`Price`)

- 미터, 채널, 통화, 무료량, 단가(`per` 단위마다), 효력일, 기존 고객 적용일, 설명을 갖는다.
- **고치지 않는다.** 모든 필드가 immutable이고 Patch와 Erase는 생성되지 않는다. 가격이 바뀌면 새 버전을 낸다. 잘못 낸 버전은 같은 날짜의 버전을 다시 내서 바로잡는다(나중에 낸 쪽이 이긴다).
- **두 날짜.** `date_effective`는 그날 이후에 시작한 고객에게, `date_existing`은 그 전부터 있던 고객에게 적용되는 날이다. 둘이 다르면 그 사이가 인상의 고지 기간이다. levyan#3의 "기존 고객에게 언제부터 적용할지도 버전이 가진다"를 이렇게 담았다.
- **어느 버전이 적용되는가.** 한 줄(미터, 채널, 통화)의 버전 중에서, 그 고객에게 그 시각까지 적용되기 시작한 버전들 가운데 `date_effective`가 가장 늦은 것이다. 같으면 나중에 낸 것이다(구현에서 정한 것). 그래서 인상 고지 기간 중에 인하가 나오면, 인상은 아직 닿지 않은 고객에게 끝내 닿지 않는다. `internal/pricing`의 `Book.InForce`와 `Book.Timeline`이 이것을 답하고, 운영자는 `levyan-admin price timeline`으로 묻는다.
- **과거로 가지 않는다.** 효력일은 낸 시각보다 이를 수 없다. 이미 쓴 기간의 금액을 바꾸는 것은 가격이 아니라 정정(#6, #9)이다(구현에서 정한 것).
- 채널은 enum이다: MoR, 국내 PG. 채널은 어댑터(코드)와 함께 생기므로 값을 늘리는 것도 코드 변경이다.

### 보존 기간 미터

- 구간마다 기간 요금을 둔다. 예: 1년까지 0원, 3년까지 10,000원, 그 위(영원히 포함) 30,000원.
- 구간은 올라가고, 마지막 구간은 상한이 없어서 모든 값이 어느 구간에든 든다. `unbounded_zero`인 미터의 0은 마지막 구간이다.
- 한 기간에 두 구간을 지났다면 각 구간의 요금을 머문 몫만큼 낸다(`Usage`의 몫). 날짜 단위 일할은 #6이 이 몫을 날짜로 계산해서 넘긴다. 기본값 "영원히"를 무료 범위에 넣을지는 가격을 정할 때 정한다.

### 금액

- 통화의 최소 단위 정수다. 원(KRW), 엔(JPY), 센트(USD). `internal/money`가 loft의 `internal/money`를 이어받았다.
- 통화가 다른 금액은 더하지 않는다. `Amount.Add`가 거부한다. 넘침도 거부한다.
- levyan이 받는 통화는 짧은 목록이다: KRW, JPY, USD, EUR. "KWR"처럼 잘못 친 세 글자가 소수 둘째 자리 통화로 받아들여지지 않게 하려는 것이다. 통화는 그것을 받는 채널이 있을 때 더한다(구현에서 정한 것).
- **반올림은 끝에서 한 번, 내림이다.** 기간 사용량은 분수인 경우가 많다(시간 가중 평균, 구간에 머문 몫). 계산은 끝까지 정확히(`math/big`) 하고, 버전 하나의 금액을 최소 단위 정수로 내린다. 의심스러우면 덜 청구한다는 원칙(levyan#1)을 따랐고, 단계마다 반올림하면 같은 사용량이 계산을 어떻게 나눴느냐에 따라 다른 금액이 되기 때문이다(구현에서 정한 것).
- 무료량은 경계까지 무료다. 무료량과 같은 사용량은 0원이고, 그 다음 단위부터 낸다. 덜 쓴 무료량은 다음 기간으로 넘어가지 않는다.

## 7. 쓰기와 trail

- 모든 쓰기는 payday의 서버를 거친다. 그래서 같은 트랜잭션 안에서 trail이 기록된다. ent로 직접 쓰지 않는다.
- 가격표의 규칙(`server/book`)은 두 스택(Walled, Ungated) 모두에 들어 있다. 제품이든 운영자든 시험이든 같은 규칙에 걸린다. 규칙은 `bare.<E>WithAddRule`이다.
- 가격 버전의 미터가 다른 제품의 것이면 sink가 거부한다(`agrees: ["meter.tenant"]`). 원장의 행도 그 워크스페이스, 미터, 시계열이 같은 제품의 것이어야 한다.
- 원장(9장)의 쓰기도 서버를 거친다. 제품이 보낸 값은 wall이 있는 스택에 그 제품으로서 쓴다.

## 8. 앱이 levyan을 부르는 길

**문제.** loft는 roster를 부를 때 roster가 생성한 Go 코드를 링크한다. 그래서 loft는 roster가 생성될 때의 payday 버전에 묶인다. loft가 levyan의 생성 코드를 링크하면 levyan의 payday 버전에도 묶인다. 제품이 늘수록 모든 제품이 levyan과 같은 날 payday를 올려야 한다.

**고른 것 (구현에서 정한 것).** levyan은 앱에게 **손으로 쓴 API**(`levyan.v1`)를 준다. 그 API의 `.proto`와, 거기서 protoc-gen-go와 protoc-gen-go-grpc만으로 생성한 Go 코드를 **자기 Go 모듈** `github.com/lesomnus/levyan/api`에 둔다.

- `api/levyan/v1/*.proto`: levyan의 스키마에서 생성한 것이 아니다. payday의 이름도 옵션도 쓰지 않는다. 식별자는 텍스트, 열거형은 API 자신의 것이다.
- `api/go.mod`: gRPC와 protobuf만 요구한다. loft가 이 모듈을 요구해도 끌려오는 것은 그 둘뿐이다. CI(`scripts/api.sh --check`)가 이 모듈의 의존 목록에 payday나 levyan 본체가 없음을 확인한다.
- levyan 본체는 `replace ./api`로 같은 저장소의 API를 쓴다. `server/api`가 API 메시지와 내부 스키마 사이를 옮기고, wall 뒤의 스택으로 읽는다.
- Go가 아닌 앱은 같은 `.proto`에서 자기 스텁을 생성한다.
- API는 필드를 더할 뿐 번호, 이름, 타입을 바꾸지 않는다. PR마다 `buf breaking`(`scripts/api.sh --breaking`)이 이를 확인한다.

**왜 이것인가.**

- **얇은 클라이언트 패키지를 levyan 본체 모듈에 두는 것은 막아 주지 못한다.** 패키지가 payday를 import하지 않아도, Go의 최소 버전 선택은 levyan 모듈의 go.mod 요구사항 전체를 앱의 모듈 그래프에 넣는다. levyan이 요구하는 payday가 더 새것이면 loft의 payday도 올라간다. 링크하지 않아도 버전은 묶인다. 그래서 모듈을 나눠야 한다.
- **`.proto`만 주고 앱이 각자 생성하게 하는 것**도 된다. 다만 앱마다 생성 설정이 생기고, payday 앱은 `pd gen`이 `proto/`를 자기 스키마로 다루기 때문에 남의 `.proto`를 넣기가 번거롭다. 생성된 Go까지 함께 주면 Go 앱은 `go get` 한 줄이고, 다른 언어는 여전히 `.proto`에서 생성할 수 있다.
- **API가 스키마와 떨어져 있어야 한다.** 엔티티는 levyan이 일하는 모양이라서 자주 바뀐다. 제품과의 약속은 그보다 오래가야 한다. 생성된 엔티티 서비스(`levyan.MeterService` 등)는 policy 표에서 누구의 몫도 아니다.
- **앱 쪽 어댑터는 앱에 둔다.** payday의 `meter.Sink`를 구현하는 어댑터는 payday를 import하므로 API 모듈에 둘 수 없다. 몇 줄이라서 앱(#14)에 둔다. payday가 청구 서비스의 클라이언트 계약을 정하게 되면(payday#186의 다음) 그쪽으로 옮긴다.

## 9. 계량: 받은 값과 원장 (구현됨, #5)

### 원장

- 원장의 행은 두 가지다. 보유량의 **읽은 값**(`Reading`: 워크스페이스, 미터, 잰 시각, 값)과 흐름의 **점**(`Point`: 시계열, 시작 시각, 시각, 그때까지의 누적값). **시계열**(`Series`)은 OpenTelemetry가 말하는 스트림 하나다. 한 프로세스가 한 워크스페이스에 대해 센 한 미터다.
- **한 번만 남긴다.** 읽은 값은 (제품, 워크스페이스, 미터, 잰 시각)이, 점은 (시계열, 시작 시각, 시각)이 행 하나다(unique 인덱스). 같은 것이 다시 오면 이미 있는 행이고, 같은 키에 다른 값이 오면 거부하고 로그(warn)에 남긴다. 둘 중 어느 쪽이 맞는지 levyan이 고르지 않는다.
- **바뀌지 않는다.** 모든 필드가 immutable이고 Patch와 Erase가 없다. 닫은 기간을 나중에 다시 물어도 같은 행에서 같은 답이 나온다.
- **trail에 남는다.** 모든 쓰기는 wall이 있는 스택을 제품으로서(서비스 키가 actor) 거친다. trail이 어느 키가 무엇을 보냈는지 답한다.
- **시각은 UTC, 마이크로초다.** PostgreSQL이 가지는 정밀도이고, payday의 `meter.Reading`이 이미 그렇게 준다.
- **워크스페이스**(`Workspace`)는 제품의 tenant(loft의 워크스페이스)를 levyan이 아는 모양이다. 처음 무언가를 받을 때 쓰고, 바꾸거나 지우지 않는다. 청구 기록은 워크스페이스가 제품에서 사라진 뒤에도 남기 때문이다. 원장 행(그리고 #6의 내역)의 field 3, 즉 payday의 두 번째 축(set)이다. #4에서 지불자가 워크스페이스 하나만 보게 좁힐 때 이 축을 쓴다(구현에서 정한 것).
- Workspace와 Series에는 Patch가 생성되지만 바꿀 수 있는 필드가 없다. protobuf-orm이 edge가 쓰는 `<E>GetKey`를 Patch와 함께만 만들기 때문이다. policy 표는 아무에게도 주지 않는다.

### 보유량

- `levyan.v1.MeteringService/Record`로 받는다. payday의 `meter.Reading`을 그대로 옮긴 것이다(워크스페이스는 제품 쪽 tenant id의 정식 텍스트).
- 거부하는 것: 모르는 미터, 흐름 미터, 미터의 것이 아닌 단위, payday tenant id가 아닌 워크스페이스, 시각이 없거나 지금보다 5분 넘게 앞선 값, 음수.
- **거부는 값 하나씩이다(구현에서 정한 것).** 같은 호출의 나머지는 남는다. 가격표에 아직 없는 미터 하나 때문에 그 워크스페이스의 다른 미터 값까지 잃지 않게 하려는 것이다. 답에 값마다 결과(남김, 이미 있음, 거부와 이유, 늦음)가 있어서, 앱 쪽 sink는 거부를 로그에 남기고 호출 실패만 다시 보내면 된다.

### 흐름 (OTLP)

- **OTLP/gRPC로 받는다(구현에서 정한 것).** 수신점은 OpenTelemetry의 `MetricsService`이고, `levyan.v1`과 같은 gRPC 서버(`server.addr`), 같은 interceptor 체인 뒤에 있다. 그래서 제품 키(`authorization: Bearer lvk_…`), policy 표, 요청 한도, 접속기록, wall이 그대로 걸린다. OTLP/HTTP로 받으면 두 번째 리스너에 그 문을 한 벌 더 만들어야 한다. 또 OTLP/gRPC는 OpenTelemetry exporter가 먼저 하는 말이라, 재시도할 코드, partial success, gzip이 그대로 맞는다. 운영자 바이너리는 이 수신점을 링크하지 않는다.
- **받는 것**은 미터 키로 이름 붙은, 단조 증가하는 누적 Sum의 정수 점이다. 점마다 `payday.tenant`(`meter.TenantAttribute`)와 시작 시각이 있어야 하고 단위는 미터의 것이어야 한다. 그 밖의 것(gauge, histogram, delta, 단조 증가가 아닌 Sum, 실수, 속성 없음, 값 없음, 모르는 미터, 보유량 미터, 다른 단위, 미래 시각)은 점 하나씩 거부하고 partial success(`rejected_data_points`, `error_message`)로 알린다.
- **시계열의 정체**는 리소스 속성, 스코프(이름, 버전, 속성), Sum의 이름, 점의 속성을 정해진 순서로 쓴 것의 SHA-256이다. 리소스에는 프로세스의 명령줄 같은 것이 들어갈 수 있어서 내용은 남기지 않고, 다이제스트와 `service.name`/`service.instance.id`만 남긴다(구현에서 정한 것).
- **증가분.** 점은 시작 시각부터 그 시각까지 센 전체다. 기간의 증가분은 점들을 시각 순으로 보면서, 시작 시각마다 그때까지의 최댓값을 넘는 만큼을 그 점의 시각이 든 기간에 센다(`internal/usage.Increase`).
  - 시작 시각이 바뀌면 리셋이다. 그 시작 시각의 첫 점은 전부 증가분이다.
  - 같은 시작 시각에서 값이 내려가면 리셋이 아니라 믿을 수 없는 값으로 보고, 내려간 만큼을 다시 세지 않는다(의심스러우면 덜 청구한다).
  - 순서가 바뀌어 와도, 두 번 와도 같다. 기간을 나눠 세어도 합은 전체와 같다. 증가분 하나는 정확히 한 기간에 든다.
- **아무것도 더하지 않는 점은 쓰지 않는다(구현에서 정한 것).** 같은 시작 시각의 더 이른 점이 이미 같거나 큰 값을 가졌다면, 그 점은 언제 왔든 어느 기간에도 아무것도 더하지 않는다. 그래서 하루 종일 쉰 시계열은 행 하나다. 셈은 모든 점을 쓴 것과 같다. 순서대로 오는 점(거의 전부)은 시계열의 시작 시각마다 집계 한 번으로 판단한다.

### 늦은 값

- 기간 닫기(#6)는 기간의 행(`Period`)을 먼저 쓰고 나서 원장을 읽는다. 원장 쓰기와 그 워크스페이스의 기간 닫기는 둘 다 워크스페이스 행을 잡는다(PostgreSQL은 `FOR UPDATE`, SQLite는 쓰기 트랜잭션이 하나씩이다). 그래서 어떤 값이 내역에도 들지 않고 표시도 되지 않는 순간이 없다. 둘 다인 값은 정정이 0으로 끝난다.
- **늦은 값**은 닫기를 시작한 기간에 대해 말하는 값이다. 읽은 값은 그 기간 안에서 쟀거나, 그 전에 쟀지만 보유 시간(아래) 안에 그 기간에 닿는 값이다. 점은 그 시각이 닫힌 기간에 있거나, 증가분을 넘겨받는 다음 점의 시각이 닫힌 기간에 있는 점이다. 받아서 `late`로 표시해 두고, #6이 나중의 내역에서 그 기간을 정정한다.

### 보유 시간 (구현에서 정한 것)

- 읽은 값은 다음 값이 올 때까지 그 값을 말하되, 최대 36시간(`usage.Hold`)이다. 읽기가 멈춘 워크스페이스는 마지막 값 뒤로 36시간까지만 청구된다. 파기한 워크스페이스, 멈춘 리더, 지워진 제품을 끝없이 마지막 값으로 청구하지 않으려는 것이다.
- 이 값은 금액 계산의 규칙이라 설정이 아니라 코드에 두고 계산 버전에 묶는다. 경보 기준(`readings.gap`)을 고쳤다고 청구가 바뀌면 안 된다.

### 빠진 값

- `levyan.readings.gaps` 루프가 매시간(`readings.every`) 돈다. 최근(`readings.recent`, 기본 7일) 안에 보유량 미터의 값을 보냈는데 마지막 값이 `readings.gap`(기본 36시간)보다 오래된 (워크스페이스, 미터)를 찾는다. 제품·미터마다 로그 한 줄(몇 개인지와 예시)과 게이지 `levyan.readings.gaps`(제품, 미터; 0도 기록해서 다시 내려간다)를 낸다. 리더가 멈춘 제품은 모든 워크스페이스가 한꺼번에 빠지므로 워크스페이스마다 한 줄씩 쓰지 않는다.
- 운영자는 `levyan-admin gap ls`로 같은 것을 바로 묻는다.
- 흐름은 일어날 때만 오므로 빠진 값을 찾지 않는다.

## 10. 금액 계산과 기간 닫기 (구현됨, #6)

### 정한 것 (Claude, 2026-10-31, #6을 맡기며)

- **청구 기간은 제품의 zone에서의 달력 월이다.** 제품마다 청구 zone(IANA 이름, 기본 UTC)이 있다. levyan#1의 열린 질문 2에 대한 답이다.
- **집계.**
  - 시간 가중 평균은 기간 **전체**에 대한 평균이다. 읽은 값과 다음 읽은 값 사이는 앞의 값이 이어진다.
  - 최댓값, 마지막 값.
  - 흐름은 증가분의 합이다.
- **무료량은 일할하지 않는다.** 기간 중에 시작한 워크스페이스에도 무료량을 다 준다. levyan#1의 열린 질문 10에 대한 답이다.
- **기간 중의 가격 변경.** `Book.Timeline`으로 기간을 나누고, 각 부분을 그 시간의 몫만큼 매긴다. 열린 질문 9에 대한 답이다.
- **보존 기간 미터.** 값은 일수로 쓴 구간이다(0은 영원히, `unbounded_zero`). 각 구간에 머문 시간의 몫을 그 구간의 요금으로 매긴다.
- **통화와 채널.** 결제 계정은 #4에서 온다. 그때까지 내역은 제품의 기본 채널(새 Product 필드)과 그 채널의 통화로 매긴다. #4가 이것을 결제 계정의 채널로 바꾼다.
- **기간 닫기.**
  - 이름 있는 루프 `levyan.periods.close`가, 늦은 값을 기다리는 유예(기본 48시간)가 지난 기간을 닫는다.
  - (제품, 워크스페이스, 기간)마다 내역(Statement) 하나를 쓴다. 미터마다 줄이 있고, 줄에는 사용량, 무료량, 청구량, 가격 버전, 금액이 있다.
  - 합계는 한 번, 내림으로 반올림한다.
  - 두 번 닫아도 한 번 닫은 것과 같다.
  - 닫은 내역은 바꾸지 않는다. 늦은 값은 나중에 열린 기간의 내역에 정정 줄로 들어가고, 그 줄은 정정하는 기간을 가리킨다.
- **제품이 부르는 API**(policy 표, 제품은 자기 tenant만 본다): 지금 기간의 사용량과 예상 금액, 내역의 Get과 List.
- **운영자 명령**: 내역 목록과 보기, 손으로 기간 닫기, 빠진 값 목록.

### 만든 것

- **제품의 조건**(`Tenant`의 overlay): `zone`, `channel`, `currency`.
  - 가격표의 규칙(`server/book`)이 Add와 Patch에서 검사한다: IANA zone, 무엇이든 파는 채널, levyan이 받는 통화.
  - Apply는 이 셋을 말할 수 없다.
  - 채널과 통화가 없는 제품은 아무것도 매기지 않고, 그 기간도 닫지 않는다.
  - 운영자는 `levyan-admin product configure`로 정한다.
- **기간**(`Period`)
  - 제품의 기간은 이어진다. 각 기간은 앞 기간이 끝난 곳에서 시작한다. 첫 기간은 원장이 시작한 달이다.
  - zone을 바꾸면 다음 기간부터 적용된다. 그 사이 기간은 첫날이 든 새 zone의 달 끝까지다. 예를 들어 서울의 1월 다음에 UTC로 바꾸면, 2월은 서울의 2월 1일부터 UTC의 3월 1일까지다. 9시간짜리 기간을 만들지 않으려는 것이다(구현에서 정한 것).
- **닫는 순서**(`server/billing`)
  1. 기간의 행을 먼저 쓴다. 이때부터 그 기간에 대해 말하는 값은 늦은 값이다(9장).
  2. 워크스페이스마다 자기 트랜잭션에서 그 행을 잡고, 내역이 아직 없으면 쓴다. 원장이 그 기간에 대해 무언가를 말하거나, 앞 기간에 대한 늦은 값이 있을 때만 쓴다. 합계가 0인 내역(무료 범위)도 쓴다.
  3. 마지막에 기간에 닫은 시각을 찍는다.
  - 중간에 멈춘 닫기는 다음에 멈춘 곳에서 이어진다. 동시에 여러 번 닫아도 워크스페이스 행의 잠금과 unique 인덱스 때문에 내역은 하나다.
  - 루프 말고 운영자도 손으로 닫을 수 있다(`levyan-admin period close`). 이것은 유예를 기다리지 않는다. trail에는 그 운영자가 남는다.
- **내역**(`Statement`): 줄과 합계를 가진다. 모든 필드가 immutable이고 Patch와 Erase가 없다.
  - 줄의 수(사용량, 무료량, 청구량, 금액)는 정확한 분수(`110/3`)로 남긴다.
  - 합계는 그 금액들을 정확히 더한 것을 한 번 내린 값이고, 내리기 전의 값(`exact`)도 남긴다.
  - 어느 계산 버전(`rating.Version`, 지금 1)으로 계산했는지 남긴다.
- **정정**
  - 늦은 행이 말하는 닫힌 기간마다, 그 기간을 지금의 원장으로 다시 계산한다. 거기서 이미 청구한 것(그 기간의 내역 줄과 그 뒤의 정정 줄)을 뺀다. 0이 아니면 정정 줄을 쓴다.
  - 정정 줄에는 정정하는 기간, 그 기간의 내역, 이미 청구한 금액, 지금의 사용량이 들어간다.
  - 이미 한 정정은 같은 산수로 0이 되어 다시 나오지 않는다. 그래서 어떤 늦은 값을 처리했는지 따로 적지 않는다(구현에서 정한 것).
  - 다른 채널이나 통화로 쓴 기간의 정정은 지금의 내역에 넣을 수 없다. 로그에 남기고 기다린다(#9).

### 계산 규칙 (`internal/usage`, `internal/rating`)

- **보유량의 사용량.**
  - 읽은 값은 다음 값까지, 최대 36시간(`usage.Hold`, 9장) 그 값을 말한다. 기간 앞의 값도 보유 시간 안에서는 기간 안으로 이어진다.
  - 시간 가중 평균은 기간 전체(마이크로초)로 나눈다. 아무 값도 말하지 않는 시간은 0이다. 그래서 기간 중에 시작한 워크스페이스는 평균이 그만큼 낮고, 무료량은 다 받는다.
  - 최댓값과 마지막 값은 그 기간 안에서 잰 값들 중에서 고른다. 기간 앞에서 잰 값이 보유 시간으로 이어져 들어온 것은 고르지 않는다(구현에서 정한 것). 서울 zone에서는 그 이어진 시간이 9시간이라, 1월 마지막 날에 줄인 자산을 2월의 최댓값으로 매기게 된다.
- **흐름의 사용량**은 기간 안의 점들이 보이는 증가분의 합이다(9장의 `usage.Increase`).
- **기간 중의 가격 변경**은 고객으로서의 시간을 나눠 각 부분을 그 몫만큼 매긴다.
  - 단위 가격: 각 부분은 기간 전체의 사용량을 그 부분의 버전으로 매긴 금액에, 그 부분이 고객이었던 시간에서 차지하는 몫을 곱한 것이다. 고객이었던 시간은 처음 원장에 나타난 때부터 기간 끝까지다. 같은 조건의 버전을 다시 내도 금액이 바뀌지 않는다(구현에서 정한 것).
  - 구간 가격: 각 부분 안에서 값이 머문 시간을, 그 부분 버전의 구간 요금으로 매긴다. 집계 방식과 상관없이 시간의 몫이다. 구간 요금은 기간 전체의 요금이기 때문이다(`PriceBand.fee`의 정의).
  - 아직 아무 버전도 적용되지 않은 시간은 매기지 않는다(가격은 거꾸로 닿지 않는다).
- **고객의 시작**(`since`): #4·#14 전까지는 그 워크스페이스의 가장 이른 원장 행(읽은 값이나 점)이다. 새 고객과 기존 고객 중 어느 가격을 적용할지(`date_effective`, `date_existing`)를 이것으로 가른다(구현에서 정한 것).
- **반올림.** 줄과 부분의 금액은 끝까지 정확하다. 내역의 합계만 한 번 내린다. 정정으로 합계가 음수가 되어도 내린다(돌려줄 것은 덜 돌려준다).

### 지금 기간의 사용량과 예상 금액 (`levyan.v1.BillingService/GetUsage`)

- 지금 열린 기간의 내역을 지금의 원장으로 닫는다면 무엇이 될지를 답한다. 닫기와 같은 코드다. 고객이 보는 값과 청구값이 같은 원장, 같은 계산에서 나온다.
- 다른 점은 하나다. 각 보유량 미터의 최신 값(보유 시간 안의 것)을 기간 끝까지 이어진 것으로 본다. 흐름은 지금까지 센 것이다. 그래서 남은 날이 말한 대로 가면, 기간을 닫은 금액은 예상 금액과 같다(구현에서 정한 것).
- 아직 정정하지 않은 늦은 값의 정정 줄도 들어간다. 다음 내역에 들어갈 것이기 때문이다.
- `GetStatement`는 id로, `ListStatements`는 워크스페이스의 내역을 최근 기간부터 준다. wall이 있는 스택을 거치므로 다른 제품의 내역은 NotFound다.
- API의 수는 소수점 아래 6자리에서 자른 10진수 문자열이다(`36.666666`). 정확한 분수는 내역에 남는다(구현에서 정한 것).

## 11. 국내 PG 채널의 경계 (라이브러리만 구현됨, #10)

levyan이 판매자인 채널(국내 PG, 나중에는 지역 PG)에게 levyan이 무엇을 묻는지를 정한 경계다. 이 경계를 결제 계정(#4), 청구서(#9), 연체(#11)에 잇는 것은 그 이슈들의 몫이고, 여기에는 없다.

### PG는 아직 정하지 않았다

- 어느 PG를 쓸지는 대표가 정할 일이고 아직 정하지 않았다. 자동결제 심사와 계약도 그때 한다(#10, loft#35).
- 토스페이먼츠는 **유력 후보**다. 그 어댑터(`internal/pg/toss`)는 **후보로서** 만들었다. PG의 시험 환경에서 시나리오를 돌려 보고, levyan이 PG에게 무엇을 묻는지를 코드로 남기려는 것이다.
- 다른 PG로 정하면 그 PG의 패키지가 같은 인터페이스(`pg.Channel`)를 구현한다. 경계를 쓰는 쪽은 PG의 이름을 모른다.

### 경계 (`internal/pg`)

- **결제수단 등록.** 지불자는 PG의 결제창에서 카드를 등록한다. levyan은 결제창을 여는 값을 주고(`StartRegistration`), 결제창이 돌려준 일회성 인증 키를 빌링키로 바꾼다(`CompleteRegistration`). 카드 정보는 levyan을 지나지 않는다. 같은 인증 키로 다시 바꾸면 같은 빌링키다(새로고침).
- **결제**(`Charge`): 결제수단, 주문 ID, 금액(원, KRW의 최소 단위), 주문명, 멱등 키.
- **조회**(`Payment`)는 주문 ID로 한다. **취소**(`Cancel`)는 부분 또는 남은 것 전부이고, 사유와 멱등 키를 받는다. **빌링키 삭제**(`RemoveMethod`)도 있다.
- **웹훅**(`Webhook`, `Recheck`)은 아래에 적었다.

### 요청의 실패와 결제의 실패 (구현에서 정한 것)

- 카드가 거절한 결제는 **결과**다. `Payment.Status`가 `Failed`이고 `Failure`에 이유가 있으며, 오류는 없다.
- 오류는 **요청의 실패**다. 오류의 종류(`pg.Kind`)는 무엇을 아는지를 말한다.
  - `Invalid`(잘못된 요청), `Auth`(levyan의 키·주소·계약), `RateLimited`(요청 한도, 점검): 아무 일도 없었다.
  - `Unknown`: 일어났는지 모른다. 시간 초과, 5xx(코드가 무엇이든), 읽지 못한 응답, 아직 처리 중인 요청이다.
  - `NotFound`: 그런 결제가 없다. 실패라기보다 답이다.
- PG의 오류 코드는 이 몇 가지로 옮기고, 코드도 그대로 남긴다. 표에 없는 코드는 HTTP 상태로 읽는다. 4xx는 `Invalid`라서 운영자가 코드를 읽게 된다. 토스는 API 버전 없이 코드를 더한다.

### 알 수 없음 (시나리오 5, 구현에서 정한 것)

- **결제.** 주문을 조회하고, `NotFound`일 때만 다시 결제한다. 같은 주문 ID에 **새 멱등 키**로 보낸다.
  - 토스는 멱등 키에 첫 응답을 15일 동안 다시 준다. 첫 응답이 오류였다면 그 오류를 준다. 그래서 같은 키로는 몇 번을 보내도 같은 답이다.
  - 이중 결제를 막는 것은 주문 ID다. 토스는 승인한 주문 ID를 다시 받지 않는다(`DUPLICATED_ORDER_ID`). 어댑터는 그 거절을 받으면 주문을 조회해서 이미 된 결제로 답한다. 금액이 다르면 `Invalid`다.
  - #10은 "주문 ID를 멱등 키로 쓴다"고 했다. 결제를 한 번으로 만드는 것은 그대로 주문 ID다. 다만 토스의 `Idempotency-Key` 헤더는 보낼 때마다의 키로 따로 둔다(예: `<주문 ID>-2`). 주문 ID를 그대로 헤더에 쓰면, 첫 응답이 오류였을 때 15일 동안 그 오류만 돌아온다.
- **취소.** 같은 취소를 같은 키로 다시 보내면, 토스가 처음에 한 일을 답한다. 그래도 모르면 결제를 조회해서 남은 금액(`Balance`)으로 판단하고, 취소되지 않았을 때만 새 키로 보낸다. 취소에는 주문 ID 같은 것이 없어서 멱등 키만이 두 번 취소를 막는다.
- 자동결제 승인은 최대 60초가 걸린다. 어댑터의 기본 시간 제한은 70초다.

### 웹훅 (구현에서 정한 것)

- **본문은 믿지 않는다.** 결제를 가리키는 웹훅이 오면 그 결제를 API로 다시 조회하고(`Recheck`), 그 답만 믿는다. 토스의 보안 체크리스트도 조회 API 교차 확인을 기본 방어선으로 꼽는다.
- **순서.**
  - 받으면(`Webhook`) 서명을 확인하고, 무엇을 가리키는지만 읽는다. 네트워크는 쓰지 않는다.
  - inbox에 저장하고 200으로 답한다. 토스는 10초 안에 200을 기다리고, 받지 못하면 3일 19시간 동안 7번 다시 보낸다.
  - 그 뒤에 다시 조회한다.
- **중복**은 토스의 `eventId`로 거른다. 결제 웹훅(`PAYMENT_STATUS_CHANGED`)에는 `eventId`가 없어서 본문의 다이제스트로 거른다. 다시 보낸 웹훅은 본문이 같다.
- **역행 금지.** 같은 결제를 두 번 관측했다면 `Payment.Progress`가 큰 쪽이 나중이고, 작은 쪽은 큰 쪽을 덮지 않는다. 웹훅의 시각으로 순서를 정하지 않는다.
- **서명.** `tosspayments-webhook-signature`는 보안 키로 만든 HMAC-SHA256이다. 문서는 어느 웹훅에 서명이 붙는지, 보안 키(16진수 64자)를 그대로 쓰는지 풀어서 쓰는지 말하지 않는다. 그래서 둘 다 받아 준다. 서명이 맞지 않는 웹훅은 거부하고, 서명이 없는 웹훅은 설정(`Signed`)이 요구할 때만 거부한다.
- **자동결제 승인은 웹훅을 보내지 않는다.** 결제의 결과는 승인 응답이나 조회로 안다. 웹훅은 그 뒤의 변경(상점관리자에서 한 취소 등)과 빌링키 삭제(`BILLING_DELETED`)를 알린다.
- 빌링키 삭제에는 다시 조회할 API가 없다. 그래서 힌트로만 받고, 다음 결제가 확인한다. 웹훅에 실려 온 빌링키는 다이제스트로만 남긴다.
- 놓친 웹훅은 대조(#13)가 찾는다.

### 빌링키

- 빌링키는 비밀이다. `pg.BillingKey`는 출력·로그·마샬에서 자기 값을 내지 않는다(마샬은 오류다). 값은 `Reveal`로만 꺼낸다.
- **저장**(#4나 #10의 연결에서 만든다).
  - levyan이 DB에 쓰기 전에 AEAD로 암호화한다. 키는 DB와 따로 배포의 비밀(`cfg.Secret`, `${file:/run/…}`)로 두고, 그 행을 연관 데이터로 묶는다. 그러면 DB 사본만으로는 아무도 결제할 수 없고, 다른 행으로 옮긴 암호문은 열리지 않는다. 암호문에 키의 이름을 남겨서 키를 교체할 수 있게 한다.
  - 옆에 다이제스트(`BillingKey.Digest`)를 둔다. 웹훅이 가리키는 결제수단을 모든 행을 열지 않고 찾기 위해서다.
  - 로그, span, 오류, 접속기록, trail의 값에 남기지 않는다. 결제 요청은 URL 경로에 빌링키가 들어간다. 그래서 어댑터에 주는 HTTP 클라이언트는 URL을 기록하면 안 된다(url.full·url.path를 남기는 HTTP 계측을 붙이지 않는다). 어댑터의 오류는 URL을 담지 않는다.
  - 지불자가 결제수단을 지우면 PG에서 먼저 지우고(`RemoveMethod`) 저장소에서 지운다.
- 토스는 발급한 빌링키를 다시 알려 주지 않는다. 잃으면 지불자가 다시 등록한다. 빌링키는 카드의 유효기간까지 쓸 수 있다.
- `customerKey`는 결제창에도 가므로 비밀은 아니지만, 남이 추측할 수 없어야 한다. 어댑터가 만들고(`lv-` 뒤에 무작위 26자), 결제 계정이 전에 쓰던 것이 있으면 그것을 받는다.

### 토스페이먼츠에서 기댄 사실 (후보, 2026-10-31 문서)

자세한 인용은 `internal/pg/toss`의 패키지 문서에 있다.

- 자동결제는 리스크 검토와 추가 계약 뒤에 쓸 수 있고, 정기 구독형 서비스를 위한 것이다([자동결제 이해하기](https://docs.tosspayments.com/guides/v2/billing)).
- 키는 API 개별 연동 키(`test_ck`/`test_sk`, `live_ck`/`live_sk`)다. 결제위젯의 시크릿 키(`gsk`)로는 주문 ID로 조회할 수 없다([API 키](https://docs.tosspayments.com/reference/using-api/api-keys)).
- 결제창(SDK v2 `requestBillingAuth`)은 카드 번호, 유효기간, 생년월일을 받고 카드 비밀번호는 받지 않는다. 국내에서 발급한 카드만 된다([자동결제 결제창 연동하기](https://docs.tosspayments.com/guides/v2/billing/integration)).
- 빌링키 발급 `POST /v1/billing/authorizations/issue`, 자동결제 승인 `POST /v1/billing/{billingKey}`(최대 60초), 조회 `GET /v1/payments/orders/{orderId}`와 `GET /v1/payments/{paymentKey}`, 취소 `POST /v1/payments/{paymentKey}/cancel`, 빌링키 삭제 `DELETE /v1/billing/{billingKey}`([코어 API](https://docs.tosspayments.com/reference)).
- 인증은 Basic이고, 시크릿 키 뒤에 콜론을 붙여 인코딩한다([인증](https://docs.tosspayments.com/reference/using-api/authorization)).
- `Idempotency-Key`는 모든 POST가 받는다. 300자까지, 15일 동안, (API 키, URL, 메서드)마다다. 같은 요청에는 첫 응답을 주고, 처리 중이면 409를 준다([멱등키 헤더](https://docs.tosspayments.com/reference/using-api/authorization#멱등키-헤더)).
- 카드는 100원 미만을 결제하지 않는다(`BELOW_MINIMUM_AMOUNT`, [에러 코드](https://docs.tosspayments.com/reference/error-codes)). 카드 결제는 1년쯤까지 취소할 수 있다([결제 취소하기](https://docs.tosspayments.com/guides/v2/cancel-payment)).
- 웹훅: [웹훅 연결하기](https://docs.tosspayments.com/guides/v2/webhook), [웹훅 이벤트](https://docs.tosspayments.com/reference/using-api/webhook-events), [보안 체크리스트](https://docs.tosspayments.com/reference/using-api/security-checklist).
- 시험 환경에서는 돈이 움직이지 않는다. `TossPayments-Test-Code` 헤더로 오류를 재현하고, API마다 분당 100건까지다([환경 설정하기](https://docs.tosspayments.com/guides/v2/get-started/environment)).

### 시험

- **가짜 PG**(`internal/pg/toss/tosstest`)는 어댑터가 쓰는 토스 API를 httptest로 흉내 낸다. 시험은 이것으로 오프라인에서 돈다. 성공, 거절, 시간 초과와 5xx 뒤의 조회, 같은 멱등 키, 부분 취소, 웹훅 재조회, 서명, 빌링키가 오류와 로그에 새지 않는지를 본다. 문서가 말하지 않는 곳에서 가짜가 어떻게 하는지는 패키지 문서에 적었다. 예를 들어 거절된 결제는 ABORTED 결제로 남는다.
- **라이브 시험**(`TestLive`)은 `TOSS_TEST_SECRET_KEY`(`test_` 키만)와 `TOSS_TEST_CUSTOMER_KEY`, `TOSS_TEST_BILLING_KEY`(또는 `TOSS_TEST_AUTH_KEY`)가 있을 때만 돈다. CI에서는 돌지 않고, 키는 저장소에 넣지 않는다. 시험용 빌링키는 토스의 API 테스트 페이지에서 시험 카드로 발급한다. levyan의 코드에는 카드 정보가 오지 않는다.

## 12. MoR 채널의 경계 (라이브러리만 구현됨, #12)

MoR이 판매자인 채널에게 levyan이 무엇을 묻는지를 정한 경계다. MoR은 자기 이름으로 지불자에게 판다. 그래서 구독의 주기, 세금, 법적 청구서, 환불, 분쟁은 MoR의 몫이고, levyan은 구독의 사본을 갖는다. 국내 PG(11장)와 달리 levyan이 판매자가 아니다. 이 경계를 결제 계정(#4), 청구서(#9), 연체(#11)에 잇는 것은 그 이슈들의 몫이고, 여기에는 없다.

### MoR은 아직 정하지 않았다

- **판매 채널은 기본이 MoR이다**(대표, 2장).
- **어느 MoR을 쓸지는 대표가 정할 일이고 아직 정하지 않았다.** 비교 기준은 #12에 있다. 한국 판매자를 받는지, 수수료, 결제창 브랜딩, 떠날 때 카드 정보를 넘겨주는지다.
- **Paddle Billing을 후보로 삼은 것은 Claude다.** Claude는 #12를 맡기며 채널 중립 인터페이스, 가짜, 그리고 Paddle의 후보 어댑터 하나를 만들게 했다. levyan#1이 대안으로 꼽은 것은 Polar다. 어댑터(`internal/mor/paddle`)는 **후보로서** 만들었다. Paddle의 샌드박스에서 시나리오를 돌려 보고, levyan이 MoR에게 무엇을 묻는지를 코드로 남기려는 것이다.
- 다른 MoR로 정하면 그 MoR의 패키지가 같은 인터페이스(`mor.Channel`)를 구현한다. 경계를 쓰는 쪽은 MoR의 이름을 모른다.
- Polar(2026-10-31 조사): levyan이 계산한 금액을 기존 구독에 청구하는 공개 API를 Polar의 문서에서 찾지 못했다. 문서에 나오는 길은 사용량 기능(이벤트와 미터)인데, levyan#1의 설계는 MoR의 사용량 기능에 기대지 않는다. Polar로 정하기 전에 확인할 것이다(15장 35번).

### 경계 (`internal/mor`)

levyan#1의 설계(제안)를 따른 것이고, Claude가 #12를 맡기며 그대로 따르게 했다.

- **금액은 levyan이 계산한다.** 기간을 닫으면 그 기간의 금액을 지불자의 구독에 일회성 청구로 넘긴다(`Charge`). MoR에게는 금액을 말하고, 값을 매길 수량은 말하지 않는다. MoR의 사용량 기능에는 기대지 않는다.
- **카드 정보는 levyan을 지나지 않는다.** 지불자는 MoR의 결제창에서 카드를 주고 구독을 시작한다. levyan은 결제창을 여는 값을 주고(`StartCheckout`), 끝난 결제창이 만든 고객과 구독을 묻는다(`CompleteCheckout`). 그 뒤에 카드로 청구하는 것은 구독 ID와 levyan의 API 키다. 비밀은 키뿐이다.
- 그 밖의 메서드:
  - **조회**: `Subscription`(구독), `Charged`(levyan의 키로 찾은 청구), `Transaction`(MoR의 ID로 찾은 거래).
  - **해지**(`Cancel`): 기간 말 또는 지금.
  - **환불**(`Refund`, `Refunded`): 사유와 멱등 키를 받는다.
  - **웹훅**(`Webhook`, `Recheck`): 아래에 적었다.
- **구독의 주인은 MoR이다.** MoR이 갱신하고, 실패한 결제를 다시 시도하고, 해지한다. levyan이 요청할 때도 있지만, 지불자가 MoR의 페이지에서 levyan에게 묻지 않고 해지할 때도 있다. levyan의 사본(`Subscription`)은 MoR의 API가 답한 것으로만 만들고, 웹훅의 본문으로는 만들지 않는다. 같은 구독의 두 답은 MoR이 마지막으로 바꾼 시각(`Subscription.Updated`)으로 순서를 정한다.

### 요청의 실패와 청구의 실패 (구현에서 정한 것)

- 오류의 종류(`mor.Kind`)는 `pg.Kind`와 이름도 뜻도 같다. 결제 원장(#9)이 두 채널을 같은 방식으로 다루게 하려는 것이다.
- 카드가 거절한 청구는 **결과**다. `Transaction.Status`가 `Failed`이고 `Failure`에 이유가 있으며, 오류는 없다.
  - Paddle에는 `on_payment_failure: prevent_change`를 보낸다(Paddle의 기본값이기도 하다). 그래서 거절된 즉시 청구는 오류로 답하고, 구독은 그대로다.
  - 다시 시도하는 청구는 새 키다.
- 오류에는 이유를 MoR의 이름 없이 말하는 값이 붙는다(`errors.Is`).
  - `ErrNoNextPeriod`: 갱신되지 않을 구독에 붙인 다음 기간 청구.
  - `ErrEnded`: 해지된 구독.
  - `ErrPastDue`: 연체된 구독. Paddle은 연체된 구독에 청구를 받지 않는다.
  - `ErrBelowMinimum`: MoR의 최소 청구 금액보다 적음.
  - `ErrOpen`: 아직 끝나지 않은 결제창.
  - `ErrTooSoon`: 아래에 적었다.
  - `ErrTwice`: 아래에 적었다.
- MoR의 오류 코드는 이 몇 가지 종류로 옮기고, 코드도 그대로 남긴다. Paddle의 요청 ID(`Error.Request`)도 남긴다. Paddle의 지원팀이 묻는 값이다.
  - 표에 없는 코드는 HTTP 상태로 읽는다.
  - 5xx는 코드가 무엇이든 `Unknown`이다.
  - 갱신 30분 안의 잠금, 승인을 기다리는 환불처럼 "지금은 아님"은 `RateLimited`다.

### 멱등 키가 없는 MoR에서 한 번만 청구하기 (시나리오 5, 구현에서 정한 것)

Paddle은 멱등 키를 받지 않는다. 문서의 어느 API도 받지 않고, 오류 목록에도 멱등 키에 관한 코드가 없다. 같은 청구를 다시 보내면 Paddle에게는 새 청구다. 그래서 키를 Paddle에 남기고, 보내기 전에 찾는다.

- **키를 청구 안에 쓴다.** 청구마다 그 청구만의 상품(non-catalog product)과 가격을 만들고, 둘의 `custom_data`에 levyan의 키(`levyan_key`)를 쓴다.
  - 상품을 고른 이유가 있다. 거래의 `details.line_items`에도, 구독의 `next_transaction`에도 상품과 그 `custom_data`가 들어 있기 때문이다. `next_transaction`에는 가격의 `custom_data`가 없다.
- **보내기 전에 찾는다.** `Charge`는 매번 구독의 다음 거래와 모든 거래(30개씩 페이지로)에서 키를 찾는다.
  - 있으면 그것으로 답하고 아무것도 보내지 않는다. 금액이 다르면 `Invalid`다.
  - 거래가 3,000개를 넘는 구독은 끝까지 찾지 않고 `Unknown`으로 답한다. 없다(`NotFound`)는 답은 끝까지 찾았을 때만 한다.
- **Settle(10분).** 찾는 것은 MoR이 이미 적어 둔 것만 본다. 응답이 오지 않은 요청은 아직 MoR 안에 있을 수 있다. 그래서 마지막으로 보낸 지 `mor.Settle`(10분)이 지나기 전에는 키가 없다는 것을 믿지 않는다.
  - 그 사이의 조회는 `Unknown`(`ErrTooSoon`)이고, 그 사이의 `Charge`는 아무것도 보내지 않는다.
  - 부르는 쪽은 보낼 때마다 그 시각을 보내기 **전에** 기록한다. 그리고 그 전의 마지막 시각을 `Charge.Sent`로 넘긴다. 처음이면 0이다.
  - 10분이 지나 `NotFound`이면 같은 키로 다시 보낸다.
- **두 번 청구된 키.** 이 규칙을 어긴 호출, 또는 MoR이 되살린 청구는 한 키를 두 거래에 남긴다. `Charged`는 첫 거래와 함께 `ErrTwice`로 답한다. 나머지는 돌려줄 것이다. 대조(#13)가 찾는다.
- **환불**에도 같은 규칙을 쓴다. 다만 Paddle의 조정(adjustment)에는 `custom_data`가 없다. 그래서 키를 사유 끝에 `[lv:키]`로 남긴다. 이 사유는 Paddle 대시보드에 보인다.
- 대안도 따져 보았다. 금액이나 시각으로 같은 청구인지 가늠하는 방법은 같은 금액의 두 청구를 가르지 못한다.

### 다음 기간 청구와 해지 예약 (#12가 짚은 함정)

청구는 지금(`Now`, 따로 된 거래로 바로 결제)이나 다음 갱신 때(`NextPeriod`, 갱신 거래에 붙임) 한다. Paddle 문서에 따르면 기간 말 해지가 예약된 구독은 `next_billed_at`이 비고 `next_transaction`도 없다. 그리고 해지될 때 다음 기간에 붙인 일회성 청구를 "자동으로 면제"한다. 이 경우를 따로 다루는 것은 #12의 범위다. Claude는 어댑터가 이때 지금 청구를 할 수 있게 하거나, 어느 쪽을 쓸지 말하게 했다. 어떻게 하는지는 구현에서 정했다.

- **갱신되지 않을 구독에 다음 기간 청구를 붙이지 않는다.** 해지나 일시정지가 예약되었거나, 일시정지되었거나, 해지된 구독이다. 보내기 전에 `ErrNoNextPeriod`로 거부한다. 부르는 쪽은 `Now`로 청구한다(구현에서 정한 것).
- **이미 붙인 다음 기간 청구의 구독에 나중에 해지가 예약될 수 있다.** 지불자가 Paddle의 페이지에서 할 수 있다. 그러면 그 청구는 어디에서도 찾을 수 없다(`NotFound`). 그때 **같은 키로** 지금 청구한다. Paddle이 버린 청구를 해지 예약이 풀릴 때 되살린다면, 그 키는 두 번 청구된 키가 된다(`ErrTwice`). 누구도 가를 수 없는 두 청구가 되지는 않는다.
- **기본은 지금 청구다**(구현에서 정한 것). 지금 청구에는 두 위험이 모두 없다. 다음 기간에 붙이면 갱신 청구서와 한 장이 되는 대신, 해지 예약에 버려질 수 있다. 또 Paddle의 갱신일(구독을 시작한 날을 따른다)까지 기다린다(15장 40번, 47번).
- 문서가 말하지 않는 것이 있다. 해지가 예약된 구독이 지금 청구를 받는지, 면제가 해지를 예약할 때인지 해지될 때인지, 예약을 풀면 청구가 돌아오는지다. 라이브 시험이 Paddle의 답을 기록한다(15장 45번).

### 결제창 (구현에서 정한 것)

- `StartCheckout`은 levyan의 서버가 거래를 만든다(`POST /transactions`). 플랜(`Config.Plan`)을 담고, 결제 계정의 ID를 `custom_data`(`levyan_account`)에 쓴다. Paddle은 그 `custom_data`를 결제가 만든 구독에 옮긴다.
- 페이지는 그 거래를 Paddle.js로 열거나(`transactionId`), 거래의 `checkout.url`(기본 결제 링크에 `?_ptxn=`)로 지불자를 보낸다. 페이지가 항목이나 `customData`를 만들지 않으므로, 페이지가 다른 결제 계정을 끼워 넣을 수 없다.
- `CompleteCheckout`은 levyan이 기록한 결제창(`Checkout`)으로만 끝낸다. 거래의 계정이 다르면 `Invalid`다. 아직 결제되지 않았거나 구독이 아직 없으면 `NotFound`(`ErrOpen`)이고, 지불자가 돌아올 때나 웹훅이 올 때 다시 묻는다.
- 플랜은 0원 월 구독으로 둘 수 있다. Paddle은 0원 기본 플랜으로 결제수단을 받고 사용량을 일회성 청구로 받는 방식을 직접 소개한다. 다만 Paddle은 0원 구독을 메일의 "결제수단 삭제" 링크로 **즉시** 해지한다(15장 41번).

### 웹훅 (구현에서 정한 것)

- **본문은 믿지 않는다.** 웹훅의 `data`는 그 일이 일어났을 때 Paddle이 찍은 모습이다. 받으면(`Webhook`) 서명과 시각을 확인하고, 무엇을 가리키는지만 읽는다. 네트워크는 쓰지 않는다. inbox에 저장하고 200으로 답한다(Paddle은 5초를 기다린다). 그 뒤에 API로 다시 묻는다(`Recheck`).
  - 거래와 조정의 웹훅은 그 거래를 조정과 함께 다시 묻는다. 환불 승인이나 분쟁은 거래가 지금 어떤지로 안다.
  - 구독의 웹훅은 구독을 다시 묻는다.
  - 나머지(고객, 주소, 결제수단 등)는 답하고 버린다.
- **서명.** `Paddle-Signature: ts=…;h1=…`이다. `h1`은 `ts + ":" + 본문`을 알림 대상(notification destination)의 비밀 키로 만든 HMAC-SHA256의 16진수다. 비밀을 교체하는 동안에는 `h1`이 여럿이고, 하나만 맞으면 된다. Paddle은 모든 웹훅에 서명하므로, 서명이 없는 웹훅은 거부한다. 비밀이 설정되지 않았으면 모두 거부하되, levyan의 실패(`Unknown`)로 답해서 Paddle이 다시 보내게 한다.
- **재전송과 재생.** `ts`는 본문과 함께 서명된다. 그래서 `ts`가 지금과 허용 오차(`Config.Tolerance`, 기본 5초. Paddle SDK의 기본값)보다 멀면 거부한다. 나중에 남이 다시 보낸 웹훅이 이렇게 걸린다. Paddle이 다시 보내는 웹훅은 그때 새로 서명하므로 받는다.
- **중복**은 `event_id`(`Notice.ID`)로 거른다. Paddle이 다시 보낸 웹훅이든 허용 오차 안의 재생이든 같은 notice이고, inbox에 이미 있다.
- **역행 금지.** 거래는 `Transaction.Progress`가 큰 쪽이 나중이다. 예정, 결제 중, 결제됨 또는 실패, 그리고 조정마다 요청과 결정 순이다. 구독은 `Subscription.Updated`가 늦은 쪽이 나중이다. 웹훅의 `occurred_at`으로는 순서를 정하지 않는다.
- Paddle은 적어도 한 번, 순서 없이 보낸다. 받지 못하면 샌드박스는 2분 동안 3번, 라이브는 이틀 동안 60번까지 다시 보낸다. 놓친 웹훅은 대조(#13)가 찾는다.

### 환불 (구현에서 정한 것)

- 환불은 Paddle의 조정이다(`POST /adjustments`, `action: refund`). 결제가 끝난(`completed`) 거래만 된다. 대상은 줄 하나의 일부(`Refund.Line`과 금액)나 줄 전부, 또는 거래 전부다.
- 금액의 뜻은 청구와 같다. 세금 모드가 `external`이면 세전 금액을 주고, Paddle이 그 세금을 더해 돌려준다.
- 라이브에서 대부분의 환불은 Paddle의 검토를 기다린다(`Requested`). 다음 조건이면 자동 승인된다.
  - 계정이 검증되어 활성이다.
  - 400 USD 이하다.
  - 잔액보다 적다.
  - 계좌 이체로 낸 결제가 아니다.
- 샌드박스는 10분마다 승인한다.
- 승인을 기다리는 환불이 있는 거래에는 다음 조정을 받지 않는다(`RateLimited`).
- 승인과 거절은 `adjustment.updated` 웹훅으로 오고, 거래를 다시 물어 확인한다.

### 비밀

- **API 키**(`Config.APIKey`)와 **웹훅 비밀**(`Config.WebhookSecret`)은 `cfg.Secret`이다(`${file:/run/…}`). 쓸 때마다 읽으므로 파일에 교체한 키가 다음 요청의 키다. 출력, 로그, 마샬에서는 참조나 `<redacted>`로만 보인다.
- 키는 헤더(`Authorization: Bearer`)로만 간다. URL에는 들어가지 않는다. 어댑터의 오류도 키를 담지 않는다. Paddle의 응답이 키를 되풀이하면 빼고 남긴다.
- 샌드박스와 라이브는 키도 주소도 따로다. `New`는 다른 환경의 키(`pdl_sdbx_`/`pdl_live_`)나 클라이언트 토큰(`test_`/`live_`)을 거부한다. 페이지로 가는 클라이언트 토큰 자리에 API 키를 넣는 것도 거부한다.
- Paddle은 공개 GitHub 저장소에서 키를 찾아 폐기한다. 이 저장소의 시험은 키 모양의 문자열을 실행 중에 만들고, 파일에 쓰지 않는다.

### Paddle에서 기댄 사실 (후보, 2026-10-31 문서)

자세한 인용은 `internal/mor/paddle`의 패키지 문서에 있다.

- 주소는 라이브 `https://api.paddle.com`, 샌드박스 `https://sandbox-api.paddle.com`이고, 각자 자기 키만 받는다. 인증은 Bearer API 키다. 버전은 `Paddle-Version: 1`로 고정한다([Quickstart](https://developer.paddle.com/api-reference/about), [Authentication](https://developer.paddle.com/api-reference/about/authentication), [Versioning](https://developer.paddle.com/api-reference/about/versioning), [Sandbox](https://developer.paddle.com/sdks/sandbox)).
- 일회성 청구는 `POST /subscriptions/{subscription_id}/charge`다([Create a one-time charge](https://developer.paddle.com/api-reference/subscriptions/create-subscription-charge), [Bill for one-time charges](https://developer.paddle.com/build/subscriptions/bill-add-one-time-charge), [on_payment_failure](https://developer.paddle.com/changelog/2023/payment-failure-behavior-update-subscription)).
  - `effective_from`은 `immediately` 또는 `next_billing_period`다.
  - 항목은 그 청구만의 가격과 상품을 `custom_data`와 함께 줄 수 있다.
  - 응답은 구독이고, 청구는 거기에 없다. 지금 청구는 거래 목록에서, 다음 기간 청구는 `next_transaction`에서 찾는다.
  - 연체된 구독과 30분 안에 갱신되는 구독은 받지 않는다.
- 해지는 `POST /subscriptions/{subscription_id}/cancel`이다(기간 말 또는 `immediately`). 해지가 예약되면 `next_billed_at`이 비고, 다음 기간 일회성 청구는 면제된다. 해지된 구독은 되살릴 수 없다([Cancel a subscription](https://developer.paddle.com/api-reference/subscriptions/cancel-subscription), [가이드](https://developer.paddle.com/build/subscriptions/cancel-subscriptions), [preview](https://developer.paddle.com/api-reference/subscriptions/preview-subscription-charge)).
- 거래는 다음으로 묻는다([List transactions](https://developer.paddle.com/api-reference/transactions/list-transactions), [Pagination](https://developer.paddle.com/api-reference/about/pagination)).
  - 하나: `GET /transactions/{transaction_id}?include=adjustments`.
  - 구독의 거래: `GET /transactions?subscription_id=…`, 30개씩, `after` 커서로.
- 결제창은 `POST /transactions`로 만든 거래이고, Paddle.js(`https://cdn.paddle.com/paddle/v2/paddle.js`)나 `checkout.url`로 연다([Create a transaction](https://developer.paddle.com/api-reference/transactions/create-transaction), [Pass a transaction to a checkout](https://developer.paddle.com/build/transactions/pass-transaction-checkout), [custom data](https://developer.paddle.com/build/transactions/custom-data)).
- 환불은 `POST /adjustments`다([Create an adjustment](https://developer.paddle.com/api-reference/adjustments/create-adjustment), [Refund or credit a transaction](https://developer.paddle.com/build/transactions/create-transaction-adjustments)).
- 오류는 `{error: {type, code, detail, documentation_url, errors}, meta: {request_id}}`이고, 5xx는 다시 시도하라고 한다. 멱등 키에 관한 코드는 없다([Errors](https://developer.paddle.com/api-reference/about/errors), [Error reference](https://developer.paddle.com/errors/overview)).
- 요청 한도는 주소당 분당 240건이다. 넘으면 429와 `Retry-After`다. 즉시 청구는 구독마다 시간당 20건, 하루 100건이다([Rate limiting](https://developer.paddle.com/api-reference/about/rate-limiting)).
- 최소 청구 금액은 0.70 USD, 0.65 EUR, 100 JPY, 980 KRW다([Supported currencies](https://developer.paddle.com/concepts/sell/supported-currencies)).
- 0원 기본 플랜으로 결제수단을 받고 사용량을 일회성 청구로 받을 수 있다. 그런 구독은 메일의 링크로 즉시 해지된다([changelog](https://developer.paddle.com/changelog/2023/zero-dollar-subscription-emails)).
- 웹훅: [How webhooks work](https://developer.paddle.com/webhooks/about/how-webhooks-work), [Handle webhook delivery](https://developer.paddle.com/webhooks/about/respond-to-webhooks), [Verify webhook signatures](https://developer.paddle.com/webhooks/about/signature-verification).
- 샌드박스에서는 돈이 움직이지 않고 시험 카드만 받는다. 시험 카드로 다음을 재현한다.
  - `4242 4242 4242 4242`: 결제된다.
  - `4000 0000 0000 0002`: 거절된다.
  - `4000 0027 6000 3184`: 처음은 결제되고, 그 뒤는 거절된다.

### 시험

- **가짜 MoR**(`internal/mor/paddle/paddletest`)은 어댑터가 쓰는 Paddle API를 httptest로 흉내 낸다. 시험은 이것으로 오프라인에서 돈다.
  - 결제창과 구독의 계정, 지금 청구와 다음 기간 청구.
  - 해지 예약에 버려지는 다음 기간 청구와 지금 청구로 바꾸기.
  - 거절은 결과로, 갱신 연체.
  - 처리한 뒤 잃은 응답, 시간 초과 뒤 Settle 전의 조회(`ErrTooSoon`), 처리 전의 5xx 뒤 같은 키로 다시 청구.
  - 두 번 청구된 키, 여러 페이지에서 키 찾기.
  - 환불의 요청·승인·남은 것 전부, 응답을 잃은 환불.
  - 본문이 낡은 웹훅의 재조회, 틀린 서명과 바꾼 시각, 허용 오차를 지난 재생, 교체 중의 두 서명.
  - 비밀이 출력·로그·오류에 새지 않는지.
- 문서가 말하지 않는 곳에서 가짜가 어떻게 하는지는 패키지 문서에 적었다. 예를 들어 거절된 지금 청구는 아무것도 남기지 않는다.
- 중요한 경로는 일부러 망가뜨려 시험이 실패하는 것을 확인했다. 망가뜨린 것은 보내기 전의 조회, Settle, 다음 기간 거부, 거절을 결과로, 5xx를 Unknown으로, 서명과 시각 확인, 키 빼기, 계정 확인, 페이지 넘기기, 환불 조회다.
- **라이브 시험**(`TestLive`)은 `PADDLE_SANDBOX_API_KEY`(`pdl_sdbx_` 키만)가 있을 때만 돈다. CI에서는 돌지 않고, 키는 저장소에 넣지 않는다.
  - `PADDLE_SANDBOX_CLIENT_TOKEN`과 `PADDLE_SANDBOX_PLAN`이 있으면 결제창을 만들고 그 주소를 남긴다. 사람이 샌드박스 결제창에서 시험 카드로 끝낸다.
  - `PADDLE_SANDBOX_SUBSCRIPTION`이 있으면 그 구독에 지금 청구와 다음 기간 청구를 하고, 부분 환불을 한다.
  - `PADDLE_SANDBOX_CANCEL=1`이면 해지 예약 뒤의 청구와 즉시 해지를 확인한다.
  - 이 시험은 아직 돌리지 않았다. 샌드박스 계정의 키가 없다.

## 13. 권한 부여와 앱으로 보내는 스트림 (구현됨, #7)

앱은 요금제 이름을 모른다. 앱이 검사하는 것은 기능과 한도이고, 그것을 levyan이 Grant로 준다. 앱은 자기 DB에 사본을 두고 쓰기의 트랜잭션 안에서 검사한다(payday#185의 `entitle`). levyan은 payday가 정한 계약 `payday.GrantService`의 원천이다.

### 정한 것 (Claude, 2026-10-31, #7을 맡기며)

- **Grant 엔티티.** 제품 tenant, 워크스페이스(앱의 tenant id), 키, 값, `valid_from`, `valid_until`, 버전, 출처를 갖는다. 출처는 운영자, 규칙, 계약, 체험 중 하나다.
  - 쓰기는 payday의 서버를 거친다. 그래서 trail에 남는다.
  - 바뀔 때마다 버전이 오르고, id는 다시 쓰지 않는다.
- **제한은 긍정 플래그로 표현한다. 그래서 Grant가 없으면 열린다(fail open).** 권한 사본에서 Grant가 없다는 것은 권한이 없다는 뜻이다. 그러니 앱의 핵심 쓰기를 Grant가 있는지에 묶지 않는다. 그렇게 하면 levyan의 빈틈 하나가 모든 워크스페이스를 잠근다.
  - 제한은 있으면 제한하는 Grant로 표현한다. 예: `loft.restricted` ON.
  - 긍정 Grant는 유료 기능과 한도에만 쓴다.
- **이번에 넣는 출처.**
  - 운영자 Grant: `levyan-admin grant add|end|ls`. trail에 그 운영자가 actor로 남는다.
  - 끼울 수 있는 규칙 훅. 결제 상태에서 나오는 규칙(지불자 없이 유예가 지난 워크스페이스, 연체)은 #4와 #11이 더한다.
- **기한과 갱신.** `valid_until`은 지금에 `grants.horizon`(기본 7일)을 더한 것이다. 이름 있는 루프 `levyan.grants.renew`가 `grants.renew`(기본 1시간)마다 살아 있는 Grant를 갱신한다. 운영자 Grant는 끝을 직접 정할 수 있다.
- **`payday.GrantService`를 공개 gRPC 서버에서, 제품에게만, policy 표의 행으로 연다.** 제품은 자기 tenant의 Grant만 본다.
  - `Watch`는 outbox(payday의 것)나 Grant와 같은 트랜잭션에 쓰는 변경 기록이 움직인다.
  - 따라가지 못하는 소비자는 스트림을 끊는다.
  - `List`는 페이지로 준다. 제품의 모든 워크스페이스 것이나 워크스페이스 하나의 것이다.
- **시험.** SQLite와 PostgreSQL에서 버전, 끝내기, 갱신, 제품 사이의 격리, `Watch`가 `ready` 다음 변경을 주는 것, 다시 연결해서 `List`하면 사본이 맞는 것, 느린 소비자를 끊는 것, payday의 `entitle.Follower`를 상대로 한 끝에서 끝까지의 시험.

### 만든 것

- **`Grant`**(domain 18): 제품(tenant), 워크스페이스(field 3, 원장과 같은 `Workspace`), 키, 값(`GrantValue`: ON, LIMIT과 그 양, UNLIMITED), `valid_from`, `valid_until`, `version`, 출처(`GrantSource`: OPERATOR, RULE, CONTRACT, TRIAL), 규칙의 이름(`rule`), 설명, `renews`. 키는 payday의 `entitle.CheckKey`를 따른다. 워크스페이스는 같은 제품의 것이어야 한다(`agrees: ["workspace.tenant"]`).
- **규칙 층**(`server/grants.Layer`, 두 스택 모두): 
  - Add는 무언가를 준다. 키, 값 하나, 시작보다 늦고 지금보다 늦은 끝, 지금보다 이르지 않은 시작, 출처, 그리고 규칙의 Grant일 때만 규칙의 이름이 있어야 한다. 버전은 요청이 무엇을 말하든 1이다.
  - Patch는 끝을 옮기거나 갱신을 멈출 뿐이다. 나머지는 바뀌지 않는다(immutable). 버전은 그 행의 다음 값이다. 끝은 지금보다 앞으로 옮길 수 없고, 이미 끝난 Grant의 끝을 뒤로 미룰 수 없다.
  - Apply는 거부한다. Erase는 끝난 지 `grants.Listed`(48시간)가 지난 Grant만 한다.
- **주고 끝내고 갱신하기**(`server/grants.Grants`): wall 없는 스택(`Ungated`)을 거쳐, 그 호출의 frame으로 쓴다. 운영자는 자기 행으로, 루프는 deployment로 쓴다.
  - 한 Grant를 바꾸는 쓰기는 그 행을 잡는다(PostgreSQL은 `FOR UPDATE`). 그래서 동시에 바꿔도 버전이 겹치지 않는다. 한 워크스페이스에 Grant를 주는 쓰기는 그 워크스페이스 행을 잡는다(`ledger.Lock`).
  - 끝이 없는 Grant는 갱신된다(`renews`). 끝은 지금(또는 시작)에서 기한만큼이다. 끝을 받은 Grant는 그때 끝나고 갱신되지 않는다.
- **규칙 훅**(`grants.Rule`): 규칙은 이름과, 제품마다 원하는 Grant의 목록(`Want`)을 준다. 루프는 원하는데 없는 것은 주고, 값이 다르면 끝내고 다시 주고, 더 원하지 않는 것은 끝낸다. #7에는 규칙이 없고 자리만 있다. #4와 #11이 `cmd.Server.Grants.Rules`에 더한다.
- **루프 `levyan.grants.renew`**: `grants.renew`마다 규칙, 갱신, 지운다(끝난 지 48시간이 지난 Grant의 soft erase). Grant 하나가 실패해도 나머지는 쓴다.
- **스트림**(`server/api.Grants`, `payday.GrantService`)
  - Grant의 변경은 `watch.outbox` 설정과 상관없이 늘 outbox에 같은 트랜잭션으로 쓴다(`cmd.Build`의 두 번째 resource recorder). drain 루프(`payday.outbox`)가 커밋된 것을 broker에 낸다. 그래서 운영자가 다른 프로세스(`levyan-admin`)에서 준 Grant도, 루프가 준 Grant도 똑같이 스트림에 닿고, 커밋과 발행 사이에 잃지 않는다.
  - `Watch`는 broker를 구독한 뒤 `ready`를 보낸다. 그 다음부터는 바뀐 Grant마다 지금 상태를 wall이 있는 스택으로 읽어서 보낸다. 다른 제품의 Grant는 NotFound라서 건너뛴다.
  - broker가 그 구독자를 위해 쌓아 두는 것(`watch.Backlog`, 64)보다 뒤처지면 구독이 닫히고 스트림은 ResourceExhausted로 끝난다. 그 소비자를 위해 더 쌓아 두지 않고, 어떤 쓰기도 기다리지 않는다.
  - `List`는 wall이 있는 스택의 페이지 목록이다(최대 500, 기본 100). `tenant_id`가 있으면 그 워크스페이스의 것만 준다. 끝나지 않은 Grant와, 끝난 지 48시간이 안 된 Grant가 들어간다.
  - `watch.broker: none`이면 levyan은 시작하지 않는다. 스트림을 줄 수 없기 때문이다.
- **운영자**: `levyan-admin grant add --workspace W (--on | --limit N | --unlimited) [--from] [--until] [--source operator|contract|trial] [--desc] [--replace] --yes PRODUCT KEY`, `grant end --yes ID`, `grant ls [--workspace] [--all] PRODUCT`.

### 구현에서 정한 것

- **값은 바뀌지 않는다.** Grant 하나는 끝날 때까지 한 값을 준다. 다른 값은 다른 Grant다. 바뀌는 것은 끝(`valid_until`)과 갱신 여부뿐이라서, "언제 무엇을 받았는가"가 행 그대로 남는다. 그래서 OFF도 없다. 빼앗는 것은 끝내는 것이다.
- **반이 남았을 때 갱신한다.** 루프는 매시간 돌지만, Grant는 기한의 반보다 적게 남았을 때만 갱신한다(DHCP가 임대를 반에서 갱신하듯). 매시간 모든 Grant를 갱신하면 Grant 하나가 일주일에 168번 바뀌고, 그때마다 levyan과 앱 양쪽에 행, trail, outbox가 쓰인다. 반에서 갱신하면 일주일에 두 번이고, 사본은 levyan이 멈춰도 적어도 기한의 반(3.5일)은 Grant를 갖는다. levyan이 오래 멈춰서 지나 버린 갱신 Grant는 돌아왔을 때 갱신한다.
- **끝난 Grant는 48시간 목록에 남고, 그 뒤 soft erase한다.** payday는 적어도 하루를 요구한다. 하루를 더한 것은 시계가 늦은 앱 때문이다. 행은 남아서 trail과 "언제 무엇을 받았는가"에 답한다.
- **주는 이 하나가 한 워크스페이스의 한 키에 주는 Grant는 한 번에 하나다.** 운영자, 계약, 체험은 모두 운영자의 손이라 한 주는 이로 친다. 같은 워크스페이스의 같은 키에 운영자 Grant가 살아 있으면 새 Grant는 거부한다. `--replace`이면 옛것을 새것의 시작에서 끝낸다. 겹침도 빈틈도 없다. 규칙의 Grant는 규칙마다 따로다. 다른 쪽끼리 겹치면 payday의 사본처럼 나중에 시작한 것이 정한다(`entitle.Decide`).
- **규칙의 Grant는 규칙의 것이다.** 운영자는 규칙의 Grant를 끝낼 수 없다(`grant end`가 거부한다). 끝내 봐야 규칙이 다음 루프에서 다시 준다. 규칙의 Grant를 없애려면 규칙이 보는 결제 상태를 바꾼다.
- **규칙이 원하는 것을 말하지 못하면 아무것도 끝내지 않는다.** 모른다는 것은 아무것도 원하지 않는다는 뜻이 아니다.
- **Grant는 과거로 가지 않는다.** 시작은 지금보다 이를 수 없고, 끝내는 것은 지금이다. 다만 "지금"은 서버 시계에서 1분(`grants.Lag`)까지 늦을 수 있다. 행을 잡느라 기다린 쓰기가 기다리기 전에 읽은 시각을 쓰기 때문이다.
- **운영자가 levyan이 모르는 워크스페이스에 Grant를 주면 그 워크스페이스 행을 쓴다.** 파일럿 고객은 측정값보다 Grant가 먼저 올 수 있다.
- **Grant의 변경만 늘 outbox에 쓴다.** 나머지 쓰기(측정값 등)는 지금처럼 `watch.outbox`를 따른다. 측정값마다 outbox 행과 삭제를 하지 않으려는 것이다.

### 앱이 지킬 것

- 앱은 payday의 `entitle.Follower`로 사본을 둔다(`docs/apps.md`). 사본에 없는 키는 권한이 없는 것이다.
- **핵심 쓰기를 Grant가 있는지에 묶지 않는다.** 제한은 `loft.restricted` 같은 플래그가 **있을 때** 막는다. 기능과 한도처럼 돈을 내야 얻는 것만 Grant가 있을 때 연다.
- 사본이 levyan에서 소식을 듣지 못하면 Grant는 `valid_until`까지만 남는다. 그 뒤에 무엇을 할지는 앱의 정책이다(payday 가이드 5장).

## 14. 앞으로의 설계 (levyan#1, 제안)

### 채널

- 채널은 결제 계정마다 하나다. 결제 계정의 나라로 정하고, 바꿀 때는 청구 기간의 경계에서 끊는다.
- 코어(계량, 가격표, 금액 계산, 권한, 원장, 대조)는 채널과 무관하다. 채널 어댑터는 누가 청구 주기를 돌리고 누가 법적 서류를 내는지만 정한다.
- 금액은 어느 채널이든 levyan이 계산한다. MoR에는 닫은 기간의 금액을 넘긴다. MoR의 사용량 기능에는 기대지 않는다. 그 경계는 12장이다.
- 지역 PG를 들이면 그 지역의 판매자가 된다(세금 등록과 신고). 수수료 차이가 그 비용보다 커질 때 들인다.

### 지불자와 결제 계정 (#4)

- 지불 정보는 roster 계정이 갖는다. 결제 계정 하나는 카드 하나와 청구서 한 장이다. 한 계정이 결제 계정을 여러 개 가질 수 있다.
- 워크스페이스는 결제 계정 하나에 연결된다. 기본은 워크스페이스를 만든 사람의 것이고, 무료 범위 안에서는 카드가 없어도 된다. 지불자를 바꾸려면 새 지불자가 받아들여야 하고, 바뀌는 시점은 기간의 경계다.
- 무료량은 워크스페이스마다 붙는다. 무료로 낼 수 있는 워크스페이스는 지불자당 N개(먼저 연결된 N개)다.
- 지불자가 없어지면 소유자에게 알리고, 유예 뒤 제한한다.

### 청구서와 정정 청구 (#9)

- 내역 다음은 청구서와 결제다. 확정한 청구서는 고치지 않고, 틀린 것은 정정 청구로 바로잡는다.
- 이력의 보존 기간은 앱이 지금 적용 중인 기간으로 잰다. 줄인 기간은 `app.retention.grace`가 지난 적용일부터다(payday 가이드의 `acme.history.keep`). levyan은 받은 값을 매길 뿐이다.

### 정합성 원칙

- 결제 요청마다 멱등 키를 둔다. 응답이 실패했다고 결제가 실패한 것은 아니다. "알 수 없음" 상태를 두고, 조회한 뒤에만 다시 시도한다.
- 웹훅은 서명 확인, inbox 저장, 응답, 처리 순이다. 이벤트 ID로 중복을 거르고, 오래된 이벤트가 새 상태를 덮지 않게 한다.
- 상태 변경과 그에 따른 작업 예약은 같은 트랜잭션에 쓴다(payday outbox).
- 주기적으로 대조한다. 의심스러우면 덜 청구한다.

## 15. 열린 질문

levyan#1에서:

1. 가격: 미터별 무료량과 단가, 무료 워크스페이스 수 N, 무료 범위에 넣을 보존 기간
2. ~~청구 기간: 달력 월인지, 어느 시간대인지(UTC 또는 워크스페이스의 zone)~~ 제품의 zone에서의 달력 월이다(Claude, 10장; 대표가 바꿀 수 있음)
3. 결제 실패 정책: 재시도 횟수, 유예 기간, 제한의 수준
4. 선정할 것: 국내 PG, MoR, 세금계산서 발행 대행
5. 라이선스
6. 배포 위치
7. 지불자가 levyan을 쓰는 길: loft가 결제 화면을 그리고 levyan을 대신 부를지, levyan이 roster의 relying party가 될지

#2·#3을 만들며 생긴 것:

8. 반올림: 버전 하나의 금액을 내림한다. 국내 PG·세금계산서의 관행(원 단위 절사)과 맞는지, 부가세는 어디서 어떻게 반올림할지
9. ~~기간 중에 가격 버전이 바뀌면 기간을 어떻게 나눌지~~ `Timeline`으로 나누고 각 부분을 시간의 몫만큼 매긴다(Claude, 10장; 대표가 바꿀 수 있음)
10. ~~부분 기간의 무료량: 기간 중에 시작한 워크스페이스의 무료량을 일할할지~~ 일할하지 않는다(Claude, 10장; 대표가 바꿀 수 있음)
11. 운영자 키의 보관: 지금은 설정의 비밀이다. 운영자가 늘거나 키를 자주 바꿔야 하면 roster의 운영자 계정으로 옮길지
12. API 모듈의 버전: `api/v0.x.y` 태그를 붙일지, pseudo-version으로 둘지. BSR에 올릴지

#5를 만들며 생긴 것:

13. 보유 시간 36시간이 맞는지. loft의 리더는 하루에 한 번(자정 UTC와 시작할 때) 재므로, 한 번 빠지면 그 뒤 12시간이 청구되지 않는다. 늘리면 파기한 워크스페이스를 그만큼 더 청구한다. #14의 생애 사건(파기됨)이 오면 그때 끊을 수 있다
14. 흐름 점의 양: exporter의 주기(기본 60초)마다 바뀐 시계열의 점이 하나씩 쌓인다. 닫은 기간의 점을 시작 시각마다 하나로 줄일지
15. 늦은 값을 언제까지 받을지: 지금은 기한이 없다. 몇 달 전 기간의 정정까지 받을지, 기한을 둘지
16. 시계열의 정체에 리소스 전체가 들어간다. `service.instance.id`를 두지 않은 앱이 같은 리소스로 두 프로세스를 띄우면, 시작 시각이 다른 두 번의 셈으로 각각 맞게 세지만 한 시계열에 섞인다. 앱 쪽 약속(payday 가이드 7장)으로 충분한지

#6을 만들며 생긴 것:

17. 고객의 시작: 지금은 그 워크스페이스의 가장 이른 원장 행이다. 기존 고객과 새 고객의 가격을 가르는 날이라, #4(연결된 날)나 #14(워크스페이스가 생긴 날)가 오면 그것으로 바꿀지
18. 음수 합계: 정정이 그 기간의 사용 금액보다 크면 합계가 음수(돌려줄 것)가 된다. 다음 기간으로 넘길지, 돌려줄지(#9)
19. 다른 통화로 쓴 기간의 정정: 지금은 로그에 남기고 기다린다. 채널을 바꾸는 시점(#4)에 어떻게 닫을지
20. 늦은 값이 많은 워크스페이스: 닫을 때마다 늦은 행이 말하는 모든 닫힌 기간을 다시 계산한다. 정정이 0이 된 기간도 다시 계산하므로, 그런 기간이 쌓이면 건너뛸 방법(예: 정정한 시점 이후의 늦은 행만 보기)이 필요한지
21. 계산 버전을 올릴 때(scenario 7): 지금은 정정을 계산하는 버전이 지금의 버전이다. 버전의 버그를 고친 뒤, 그 버전으로 닫은 기간을 모두 다시 계산할지 늦은 행이 있는 기간만 할지
22. 최댓값과 마지막 값은 그 기간 안에서 잰 값들에서만 고른다. 기간 안에서 잰 값이 하나도 없으면(리더가 기간 시작 직전에 멈춤) 0이 된다. 그 경우 이어진 값을 쓸지

#10(PG 어댑터 라이브러리)을 만들며 생긴 것:

23. PG 선정과 계약: 토스페이먼츠로 할지. 자동결제 심사에서 levyan의 청구(매달 쓴 만큼 금액이 달라지는 청구)가 정기 구독형으로 인정되는지. 카드사 계약이 카드 비밀번호를 요구하면 결제창을 쓸 수 없고, 그러면 카드 정보가 levyan을 지나야 한다. 지금의 경계는 그것을 허용하지 않는다
24. 웹훅 서명: 결제 웹훅(`PAYMENT_STATUS_CHANGED`)과 `BILLING_DELETED`에도 서명이 붙는지, 보안 키를 그대로 쓰는지 풀어서 쓰는지. 토스에 묻거나 첫 실제 웹훅으로 확인한 뒤, 붙는다면 `Signed`를 켜고 한쪽 읽기를 지운다
25. 100원 미만의 청구: 카드는 100원 미만을 결제하지 않는다. 다음 기간으로 넘길지, 받지 않을지(#9)
26. 거절의 다음: 잔액 부족처럼 나중에 될 수 있는 거절과, 분실·정지처럼 새 카드가 필요한 거절을 #11이 나눠야 하는지. 지금은 모두 `Declined`이고 PG의 코드가 남는다
27. 빌링키를 암호화하는 키: 설정의 비밀로 둘지 KMS에 둘지, 교체 주기(#4)
28. 주문 ID와 멱등 키: #10의 "주문 ID를 멱등 키로 쓴다"를, 결제를 한 번으로 만드는 것은 주문 ID로 하고 토스의 `Idempotency-Key` 헤더는 보낼 때마다의 키로 나눠 받았다(11장). 이렇게 나눈 것이 맞는지

#7을 만들며 생긴 것:

29. 규칙의 제한을 운영자가 풀 길: 지금은 없다. OFF 값이 없고, 운영자는 규칙의 Grant를 끝낼 수 없다. 잘못 제한된 워크스페이스는 결제 상태를 고쳐서 푼다. 운영자의 면제(exemption)를 둘지
30. 갱신 주기: 기한의 반에서 갱신한다(구현에서 정한 것). Claude가 정한 "매시간 갱신"을 글자 그대로 따를지
31. 끝난 Grant를 목록에 남기는 기간: 48시간. 시계가 많이 늦은 앱이 있다면 늘릴지
32. 두 규칙이 한 키에 다른 값을 줄 때: 지금은 나중에 시작한 것이 정한다. 규칙끼리 합칠 방법(예: 제한은 하나라도 원하면 ON)을 정할지
33. 스트림의 지연은 outbox drain의 주기(`watch.drain_every`, 기본 1초)다. 더 빨라야 하는지
34. 여러 프로세스: `watch.broker: memory`이면 drain을 도는 프로세스의 스트림만 변경을 듣는다. 두 프로세스 이상이면 `postgres` broker가 꼭 필요하다

#12(MoR 어댑터 라이브러리)를 만들며 생긴 것:

35. MoR 선정: Paddle로 할지, Polar나 다른 MoR로 할지. #12의 비교 기준은 한국 판매자를 받는지, 수수료, 결제창 브랜딩, 떠날 때 카드 정보다(36~38번). Polar에서는 levyan이 계산한 금액을 구독에 청구하는 공개 API를 찾지 못했다. 문서에 나오는 길은 사용량 기능이다(12장)
36. 판매자(seller of record) 계약: MoR이 판매자이므로 계약은 MoR과 levyan의 사업자 사이다. 한국 사업자로 가입과 심사를 받는지, 정산의 통화와 주기, 허용 업종. levyan을 쓰는 제품마다 심사를 받아야 하는지. levyan#1은 MoR 계정을 제품마다 두지 않는다
37. 결제창의 브랜딩: Paddle의 결제창, 영수증, 청구서, 카드 명세서에 판매자로 무엇이 보이는지. MoR이 판매자이므로 Paddle의 이름이 보인다. levyan이나 loft의 브랜드를 어디까지 넣을 수 있는지. 샌드박스의 결제창에는 "Test Mode" 표시가 붙는다
38. 떠날 때의 카드 정보: MoR을 떠날 때(시나리오 11, 지역 PG 도입) 고객의 카드 정보를 넘겨받을 수 있는지. 2026-10-31의 Paddle 문서에서는 찾지 못했고, Paddle에 물어야 한다. 받지 못하면 levyan#1의 설계대로 새 고객부터 새 PG로 받고, 기존 고객은 갱신 때 다시 등록하게 한다
39. 세금 모드: levyan이 계산한 금액이 세전인지(`external`, 기본으로 둠) 세금을 포함하는지(`internal`). 가격표의 단가가 무엇을 뜻하는지와 같은 질문이고, 8번(부가세의 반올림)과도 이어진다
40. 지금 청구할지 다음 기간에 붙일지: 기본을 지금 청구로 두었다(12장, 구현에서 정한 것). 다음 기간에 붙이면 갱신 청구서와 한 장이 된다. 대신 해지 예약에 버려질 수 있고, Paddle의 갱신일까지 기다린다. 거래마다 붙는 수수료의 차이도 기준이다
41. 0원 플랜의 즉시 해지: Paddle은 0원 구독을 메일의 "결제수단 삭제" 링크로 바로 해지한다. 그러면 닫지 않은 기간의 사용 금액은 그 구독으로 청구할 수 없다. 지불자가 없어진 워크스페이스로 다룰지(#4·#11), 그 기간의 금액을 어떻게 할지, 플랜에 0원이 아닌 기본 요금을 둘지
42. 최소 청구 금액: Paddle은 0.70 USD, 0.65 EUR, 100 JPY, 980 KRW보다 적게 청구하지 않는다(`ErrBelowMinimum`). 다음 기간으로 넘길지, 받지 않을지(#9, 25번과 같은 질문)
43. 환불의 승인: 라이브의 환불은 대부분 Paddle의 검토를 기다리고, 기다리는 환불이 있으면 다음 환불을 받지 않는다. 정정과 환불(#9, 시나리오 13)이 "요청됨"과 "거절됨"을 어떻게 다룰지
44. Settle 10분: 응답이 오지 않은 청구는 10분 동안 다시 보내지 않고, 그 동안의 "없음"을 믿지 않는다(구현에서 정한 것). Paddle이 그보다 늦게 기록할 수 있다면 늘려야 한다. 라이브 시험과 운영의 기록으로 확인할지
45. 해지가 예약된 구독: Paddle 문서가 말하지 않는다. 지금 청구를 받는지(`subscription_locked_pending_changes`로 거절할 수도 있다), 다음 기간 청구를 언제 면제하는지(예약할 때, 또는 해지될 때), 예약을 풀면 청구가 돌아오는지. 라이브 시험(`PADDLE_SANDBOX_CANCEL=1`)이 기록한다. 지금 청구도 거절한다면, 마지막 기간의 금액은 해지가 예약되기 전에 청구해야 한다
46. 웹훅 시각의 허용 오차: 5초(Paddle SDK의 기본값)다. levyan의 시계가 그보다 어긋나면 진짜 웹훅도 거부된다. Paddle이 다시 보내지만, 계속 어긋나면 받지 못한다. 늘릴지
47. Paddle의 갱신일과 levyan의 기간: Paddle의 갱신일은 구독을 시작한 날을 따르고, levyan의 기간은 제품 zone의 달력 월이다(10장). 지금 청구하면 둘은 상관없다. 다음 기간에 붙이거나 0원이 아닌 기본 요금을 둔다면, 갱신일을 달의 경계로 옮길지
48. 환불의 키를 사유에 남기는 것: Paddle의 조정에는 `custom_data`가 없어서 사유 끝에 `[lv:키]`를 남겼다(구현에서 정한 것). Paddle 대시보드에 보인다. 괜찮은지

## 16. 구현 계획

1단계, 결제 없이 loft 파일럿의 그림자 청구까지: #2 뼈대, #3 가격표, #4 결제 계정과 지불자, #5 계량, #6 금액 계산과 기간 닫기, #7 권한 부여와 스트림(13장), #14 loft 연동, #8 그림자 청구.

2단계, 국내 판매: #9 청구서·정정·결제 원장, #10 국내 PG 어댑터(경계와 후보 어댑터는 11장, 연결과 PG 시험 환경의 완료 기준은 남았다), #11 연체·유예·제한, #13 대조와 경보.

3단계, 해외 판매: #12 MoR 어댑터(경계와 Paddle 후보 어댑터는 12장, 연결과 MoR 샌드박스의 완료 기준인 시나리오 2, 3, 6, 11은 남았다).

다른 저장소: payday#184(계정 호출, payday#190으로 들어갔다), payday#185(권한 사본, payday#188로 들어갔다), payday#186(미터: 선언과 `meter.Reader`는 payday#187로 payday 027b311에 들어갔다), roster#210(계정 상태 확인과 동기 스트림). levyan은 payday 0d342d8을 쓴다.
