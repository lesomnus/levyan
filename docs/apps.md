# 앱이 levyan을 쓰는 법

levyan이 과금하는 앱(제품)이 할 일이다. 첫 제품은 loft다. 왜 이렇게 생겼는지는 [design.md](design.md)에 있다.

## 1. 키와 연결

- 운영자가 제품을 세우고 서비스 키를 발급한다(`levyan-admin key issue`). 키는 발급할 때 한 번만 나온다. 앱은 비밀로 보관한다.
- 모든 호출에 `authorization: Bearer lvk_…`를 붙인다. 앱이 부르는 것은 둘이다.
  - `levyan.v1`: 손으로 쓴 API다. Go 앱은 `github.com/lesomnus/levyan/api` 모듈을 쓴다. 이 모듈은 gRPC와 protobuf만 요구한다.
  - payday의 `payday.GrantService`: 권한(3장). payday 앱이면 이미 `pdpb`에 있다.
- 한 연결로 둘 다 부른다. 스트림(`GrantService.Watch`)이 있으므로 keepalive를 켜서 연결한다. 그래야 프록시의 idle timeout이 조용한 스트림을 끊지 않는다. levyan의 `server.keepalive.min_time`은 앱의 keepalive 주기보다 짧아야 한다.

```go
conn, err := grpc.NewClient(addr,
	grpc.WithTransportCredentials(creds),
	grpc.WithPerRPCCredentials(bearer(key)), // authorization: Bearer lvk_…
	grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 5 * time.Minute, Timeout: 20 * time.Second}),
)
```

## 2. 계량: sink

앱은 payday의 미터(`meters`, payday#186)로 잰 것을 levyan에 보낸다.

- **보유량**: payday의 `meter.Reader`가 잰 `meter.Reading`을 `levyan.v1.MeteringService/Record`로 보낸다. 이것이 앱 쪽의 `meter.Sink`다. 몇 줄이라 앱에 둔다(design 8장).
  - 워크스페이스는 앱의 payday tenant id의 정식 텍스트다.
  - 답은 값마다 하나다. `OUTCOME_REFUSED`는 로그에 남기고 다시 보내지 않는다. 호출 자체가 실패했을 때만 그대로 다시 보낸다.
  - 워크스페이스를 파기하기 전에 마지막으로 재서 보낸다.
- **흐름**: 청구용 `MeterProvider`로, 같은 주소에 OTLP/gRPC로 보낸다. 점마다 `payday.tenant` 속성을 붙이고, 프로세스마다 `service.instance.id`를 둔다.

자세한 것은 README의 "계량: 보유량과 흐름"에 있다.

## 3. 권한: 사본을 두고, 트랜잭션 안에서 검사한다

levyan은 워크스페이스마다 Grant를 준다. Grant는 키, 값(켜짐, 한도, 무제한), 유효 기간을 갖는다. 앱은 payday의 `entitle`로 자기 DB에 사본을 두고, 쓰기의 트랜잭션 안에서 검사한다(payday의 `docs/guide/entitlements.md`).

### 사본 켜기

스키마에 선언하면 `pd gen`이 사본 엔티티를 앱에 쓴다. domain은 앱이 비워 둔 번호다.

```proto
option (payday.app) = {entitlements: {domain: 43}};
```

`entitle.Follower`를 levyan에 붙인다. lease 아래에서 한 프로세스가 돈다.

```go
s.Follower = &entitle.Follower{
	Source: pdpb.NewGrantServiceClient(conn), // levyan, 제품 키로
	Copy:   pd.EntitlementCopy(drv, content),
}
s.Spin = append(s.Spin, s.Follower)
```

- follower는 `Watch`를 열고, `ready`를 받은 뒤 전체 목록(`List`)과 맞춘다. 그 뒤로는 변경마다 사본에 쓰고, 한 시간마다 다시 맞춘다.
- 스트림이 끊기면 다시 열고 다시 맞춘다. levyan은 따라오지 못하는 소비자의 스트림을 끊는다(ResourceExhausted). follower는 그것도 다른 끊김처럼 다룬다.
- 새 워크스페이스를 만들었으면 다음 비교를 기다리지 말고 `s.Follower.Sync(ctx, tenant)`를 부른다.
- 앱이 모르는 tenant의 Grant는 아무것에도 쓰이지 않는다.

### 검사

```go
// 한도: 트랜잭션 안에서, 잠그고, 센 뒤에
limit, err := entitle.Limit(ctx, pd.Entitlements(drv), "loft.seats")
if !limit.Allows(held + 1) { /* FailedPrecondition */ }

// 기능
on, err := entitle.Has(ctx, pd.Entitlements(drv), "loft.export")
```

### 핵심 쓰기를 Grant가 있는지에 묶지 않는다

사본에 없는 키는 권한이 없는 것이다. 그러니 **levyan에 빈틈이 생기면 Grant가 주던 것만 사라진다.** Grant가 갱신되지 못하고 끝나거나, 스트림이 돌아오지 않는 경우다. 이 성질은 유료 기능에는 맞지만, 앱의 핵심 쓰기를 Grant에 묶으면 levyan의 빈틈 하나가 모든 워크스페이스를 잠근다.

- **제한은 있으면 막는 Grant로 검사한다.** levyan은 제한을 `loft.restricted` ON 같은 Grant로 준다(지불자 없이 유예가 지난 워크스페이스는 4장, 결제가 실패한 채 유예가 지난 청구서의 워크스페이스는 6장). 둘 다 같은 키다. 제한의 두 번째 단계인 읽기 전용은 `loft.readonly` ON이다(아래 "제한의 두 단계"). 앱은 그 Grant가 **있을 때** 막는다.

  ```go
  restricted, err := entitle.Has(ctx, pd.Entitlements(drv), "loft.restricted")
  if err != nil {
  	return err
  }
  if restricted {
  	return status.Error(codes.FailedPrecondition, "this workspace is restricted until its billing is settled")
  }
  ```

- **하지 않는 것**: `entitle.Has(ctx, r, "loft.writes")`가 거짓이면 쓰기를 막는 것. levyan이 멈추면 모든 워크스페이스가 막힌다.
- **긍정 Grant는 유료 기능과 한도에만 쓴다.** 내보내기, 좌석 수, 로봇 수처럼 돈을 내야 얻는 것이다. 무료 범위는 Grant 없이 앱이 정한다.

### 제한의 두 단계: `<제품>.restricted`와 `<제품>.readonly`

결제되지 않은 워크스페이스는 두 단계로 막는다(대표, 2026-11-01). 두 Grant는 각자의 규칙이 따로 주고, 앱은 둘을 따로 검사한다. 어느 것이든 **있을 때** 막는다.

| 단계 | 키 | 언제 오는가 | 앱이 하는 것 |
| --- | --- | --- | --- |
| 1단계, 제한 | `<제품>.restricted` (`loft.restricted`) | 결제가 실패한 청구서의 유예가 끝날 때(행이 바로라고 하면 첫 실패에), 지불자가 없어진 지 30일 뒤 | 그 워크스페이스가 청구되는 것을 늘리는 쓰기를 막는다. 일상 업무는 계속된다 |
| 2단계, 읽기 전용 | `<제품>.readonly` (`loft.readonly`) | 그 청구서의 첫 실패 60일 뒤, 지불자가 없어진 지 60일 뒤. 1단계보다 이르지 않다 | 아래의 것 말고는 그 워크스페이스의 모든 쓰기를 거부한다 |

**`<제품>.readonly`가 있는 워크스페이스에서 앱이 받는 것**은 이것뿐이다.

- **읽기.** 모든 읽기.
- **내보내기와 내려받기.** 워크스페이스의 데이터를 내보내고, 파일을 내려받는 것.
- **결제.** 지불자와 결제의 모든 것. 결제 계정을 만들고 고치는 것, 결제창(`StartCheckout`, `CompleteCheckout`), 지불자가 되는 것(제안, 받아들이기와 거절하기, `Use`, `Stop`)과, 그 화면이 앱에 쓰는 것.
- **닫기.** 워크스페이스를 닫는 것.
- **떠나기.** 구성원이 워크스페이스를 떠나는 것.

그 밖의 쓰기는 모두 거부한다(FailedPrecondition). 지키는 것은 앱이다. levyan은 키를 줄 뿐이다.

```go
readonly, err := entitle.Has(ctx, pd.Entitlements(drv), "loft.readonly")
if err != nil {
	return err
}
if readonly && !allowedWhenReadonly(method) { // 읽기, 내보내기·내려받기, 결제, 닫기, 떠나기
	return status.Error(codes.FailedPrecondition, "this workspace is read-only until its billing is settled")
}
```

- **각자 검사한다.** `<제품>.readonly`는 `<제품>.restricted`보다 먼저 오지 않지만, 앱은 `<제품>.readonly` 하나만 보고도 읽기 전용으로 다룬다. 둘이 한 트랜잭션에 오는 것은 아니다.
- **풀리는 때.** 청구서가 결제되면 두 Grant가 함께 끝난다(levyan이 1분 안에 끝내고, 스트림으로 온다). 지불자가 없던 워크스페이스는 누가 받아들이면 levyan의 Grant 루프(1시간)에서 둘 다 끝난다.
- **언제 오는지**는 `GetPayer`의 `payer.restricted_from`·`payer.readonly_from`(지불자 없음, 4장)과 `payer.dunning[]`의 `restricted_from`·`readonly_from`(청구서, 6장)이 말한다. 그 때가 지났는데 Grant가 아직 오지 않았으면 배너만 보이고 막지 않는다.
- 기간(30일, 60일)은 levyan 배포의 정책 표의 값이고(`dunning.kinds`), 배포가 바꿀 수 있다.

### levyan이 멈췄을 때

- 사본은 마지막으로 들은 대로 답한다. 소식을 못 들었다고 지우지 않는다.
- Grant는 저마다 끝(`valid_until`)이 있다. 갱신되는 Grant의 끝은 마지막 갱신에서 7일(`grants.horizon`)이고, 반이 남으면 갱신된다. 그래서 levyan이 멈춰도 사본은 적어도 3.5일 동안 Grant를 지킨다.
- 끝이 지난 Grant를 어떻게 할지는 앱의 정책이다. 하루 더 봐주려면 `entitle.All`과 `entitle.Decide`로 읽는다(payday 가이드 5장).

### 키

키는 앱의 이름으로 시작하는 점으로 이은 소문자 단어다(`entitle.CheckKey`): `loft.export`, `loft.seats`, `loft.restricted`. 어떤 키에 무엇을 주는지는 levyan이 정하고, 앱은 검사할 키만 안다. 지금 levyan이 주는 것은 운영자의 Grant(파일럿, 계약, 체험)와 제한의 `loft.restricted`, `loft.readonly`다. 지불자 없이 유예가 지난 워크스페이스(4장)와, 결제가 실패한 채 유예가 지난 청구서의 워크스페이스(6장)에 `loft.restricted`를, 그 60일 뒤에 `loft.readonly`를 준다(위의 "제한의 두 단계").

## 4. 누가 내는가: 워크스페이스의 지불자

지불자는 roster 계정이다. 워크스페이스마다 한 번에 한 명이고, 결제 계정으로 낸다. 앱은 levyan에 두 가지를 알린다.

### 앱이 알리는 것 (`levyan.v1.WorkspaceService`, 제품 키로)

- **`Arrive(workspace, creator, created)`**: 워크스페이스를 만들 때마다 부른다. 만든 사람이 만든 때부터 지불자다. 다시 불러도 같으므로, 실패하면 그대로 다시 보낸다. levyan을 붙이기 전에 있던 워크스페이스는 한 번씩 `created`에 만든 때를 넣어 부른다.
- **`Leave(workspace, account)`**: 계정이 워크스페이스를 떠날 때 부른다(roster가 계정을 지운 경우 포함). 그 계정이 지불자였다면 지금부터 지불자가 없다.
- **`Offer(workspace, account)`**: 소유자가 다른 계정에게 지불자가 되어 달라고 할 때 부른다. 받아들이는 것은 그 계정이 한다(아래).
- **`GetPayer(workspace)`**: 지금과 다음 기간의 지불자, 열린 제안, 지불자가 없는 때(`payerless_since`)와 제한되는 때(`restricted_from`)와 읽기 전용이 되는 때(`readonly_from`), 그 워크스페이스를 청구한 청구서 중 결제가 실패한 것(`dunning`, 6장). 소유자에게 "지불자가 없습니다, 언제부터 제한됩니다"를 보여 주는 것은 앱이다.

### 지불자의 화면 (`levyan.v1.PayerService`, 사용자를 대신해서)

결제 화면은 앱이 그린다. 앱은 로그인한 사용자의 roster 계정을 자기 키 옆에 붙여 levyan을 부른다.

```go
ctx = metadata.AppendToOutgoingContext(ctx,
	"authorization", "Bearer "+key,
	"levyan-account", account.String(), // 앱이 roster로 로그인시킨 계정
)
payers := levyanv1.NewPayerServiceClient(conn)
bas, err := payers.ListBillingAccounts(ctx, &levyanv1.ListBillingAccountsRequest{})
```

- 부를 수 있는 것: 결제 계정 목록·만들기·고치기·읽기(`GetBillingAccount`), 받은 제안 목록, 받아들이기·거절하기, 내는 워크스페이스 목록, 다른 결제 계정 쓰기(`Use`), 그만 내기(`Stop`), 결제창(`StartCheckout`, `CompleteCheckout`, 아래). 그 밖의 메서드는 거부된다.
- 받아들이기, `Use`, `Stop`은 지금 기간의 끝에서 바뀐다.
- **결제 계정의 채널과 통화는 levyan이 정한다.** 앱은 나라만 보낸다. 그 나라를 어느 채널과 통화로 파는지는 levyan 배포의 설정이고(`payers.channels`, 국내는 기본이 MoR과 원이다), 만든 뒤에는 바뀌지 않는다. 화면에는 `CreateBillingAccount`와 `ListBillingAccounts`가 답한 `channel`과 `currency`를 그대로 보여 준다. 나라에서 통화를 짐작하지 않는다.
- **믿음.** 앱이 계정을 보증한다. levyan은 그 계정의 지금 상태를 roster에 묻고, 정지되거나 지워진 계정은 거부한다(PermissionDenied). roster에 물을 수 없으면 Unavailable이니 다시 보낸다. levyan에 roster가 설정되지 않았으면 FailedPrecondition이다.
- 제안과 연결은 그 호출을 보낸 제품의 것만 보인다. 결제 계정은 제품과 상관없이 그 계정의 것이다.

### 결제수단: MoR(Paddle)의 구독

MoR 채널의 결제 계정(`channel`이 `CHANNEL_MOR`)은 지불자가 Paddle의 결제창에서 시작한 구독으로 수금한다. 카드는 Paddle에게 가고, 앱에도 levyan에도 오지 않는다.

1. **`StartCheckout(billing_account, success_url)`** (지불자로서). `success_url`은 결제가 끝나면 Paddle이 지불자를 보낼 앱의 주소다. 답의 `checkout.window`로 페이지가 결제창을 연다.

   ```js
   // window.script를 불러온 뒤
   if (w.environment === "sandbox") Paddle.Environment.set("sandbox");
   Paddle.Initialize({ token: w.client_token });
   Paddle.Checkout.open({
     transactionId: w.transaction,
     settings: { successUrl },
     // 지불자의 이메일이 있으면 나라와 함께 미리 채운다. Paddle은 국내 카드,
     // 카카오페이, 네이버페이를 원화이고 한국 주소일 때만 보여 준다.
     customer: { email, address: { countryCode: w.country } },
   });
   ```

   페이지를 그리지 않고 `window.url`(Paddle의 결제 페이지)로 보내도 된다. 다시 불러도 같은 결제창이 온다(열려 있는 동안). 이미 구독이 있는 결제 계정, MoR 채널이 아닌 결제 계정, MoR이 설정되지 않은 levyan은 FailedPrecondition이다.
2. **`CompleteCheckout(checkout)`**: 지불자가 `success_url`로 돌아오면 부른다. `checkout`은 levyan의 결제창 id(`checkout.id`)다. 아직 `OPEN`이면 잠시 뒤 다시 부른다(Paddle이 구독을 만드는 중이다). levyan은 Paddle의 알림으로도 끝내므로 지불자가 돌아오지 않아도 구독은 잡힌다. `CANCELED`면 새로 `StartCheckout`한다.
3. **결제 화면**: `GetBillingAccount(id)`의 `collection`(수금할 수 있는지와 이유)과 `subscription`(상태, 다음 청구, 예정된 해지, 결제수단의 종류·브랜드·끝 네 자리), 그리고 `dunning`(결제가 실패한 청구서, 6장)을 그린다. `collection.state`가
   - `COLLECTABLE`: 정상. `ends`가 있으면 그때 해지되거나 일시정지된다(지불자가 Paddle에서 예약했다).
   - `NO_SUBSCRIPTION`: 결제수단을 등록하게 한다(`StartCheckout`).
   - `PAST_DUE`: 결제가 실패해 Paddle이 다시 시도하는 중이다.
   - `NEEDS_ACTION`: 결제가 지불자를 기다린다(국내 카드의 앱 승인, 은행의 확인). Paddle이 보낸 안내를 따르게 한다.
   - `PAUSED`, `CANCELED`: 구독이 멈췄거나 끝났다. 끝났으면 새로 `StartCheckout`할 수 있다.
4. **워크스페이스의 배너**: 제품 키로 부르는 `GetPayer(workspace)`의 `payer.collection`이 지금 내는 결제 계정의 상태다. 결제수단은 보이지 않는다. `COLLECTABLE`이 아니면 소유자에게 알린다. 결제가 실패한 청구서는 `payer.dunning`이고, 무엇을 언제 막을지는 levyan이 Grant로 정한다(6장, 3장).

### 지불자가 없으면

마지막 지불자가 떠난 지 30일이 지나면 levyan이 `<제품>.restricted` ON을 준다(loft는 `loft.restricted`). 60일이 지나면 `<제품>.readonly` ON도 준다(`loft.readonly`, 3장의 "제한의 두 단계"). 30일과 60일은 levyan 배포의 정책 표에서 지불자 없는 워크스페이스의 행(`dunning.kinds.payerless`)의 기본값이고, 행은 바로 제한하라고 할 수도 있다. `GetPayer`의 `restricted_from`과 `readonly_from`이 그때를 말한다. 앱은 3장처럼 **그 Grant가 있을 때** 막는다. 누가 받아들이면 두 Grant는 끝난다.

## 5. 워크스페이스의 생애 사건: 닫힘, 복원, 파기

닫힌 워크스페이스는 닫혀 있는 동안 청구하지 않는다. 되돌리면 닫혀 있던 구간까지 청구하고, 닫힌 채 파기하면 닫힌 때부터 청구하지 않는다(대표, 2026-11-01). 시스템은 복구 기간을 청구하지 않고, 되돌린 워크스페이스는 닫힌 적이 없었던 것처럼 청구한다. 파기한 워크스페이스는 그때 영원히 끝난다(Claude, design 14장의 생애 사건). levyan이 그것을 알려면 앱이 알려야 한다. 셋 다 제품 키로 부르는 `levyan.v1.WorkspaceService`의 호출이다. loft는 이미 `Restore`를 보낸다. 앱이 보내는 것은 바뀌지 않았고, 바뀐 것은 levyan이 매기는 금액이다.

| 호출 | 언제 보내는가 | `at` |
| --- | --- | --- |
| `Close(workspace, at)` | 워크스페이스를 닫는 트랜잭션이 커밋될 때. 소유자가 닫든, 운영자가 닫든, 구성원이 아무도 남지 않아 배포가 닫든 같다 | 닫은 시각. 앱이 그 워크스페이스에 적은 닫은 시각 그대로 |
| `Restore(workspace, at)` | 닫힌 워크스페이스를 되돌리는 트랜잭션이 커밋될 때 | 되돌린 시각 |
| `Destroy(workspace, at)` | 파기하기로 하고, 마지막 측정을 levyan에 넘긴 뒤, 무엇이든 지우기 전에 | 마지막 측정이 끝난 뒤의 시각 |

- **`at`은 반드시 보낸다.** 그 사건이 앱에서 일어난 시각이다. 비우면 InvalidArgument다. levyan은 사건을 받은 순서가 아니라 `at`의 순서로 읽는다. 그래서 늦게 보낸 사건도 제자리에 들어가고, 오래된 사건이 새 사건을 되돌리지 않는다. 다시 보내는 것은 같은 `at`으로 보낸다. 다른 `at`은 다른 사건이다.
- **사건을 바꾸는 트랜잭션과 같은 트랜잭션에 outbox 행을 쓰고, 커밋된 뒤에 보낸다.** 닫기가 롤백되면 `Close`도 나가지 않아야 하고, 닫기가 커밋되면 `Close`는 언젠가 나가야 한다. 되돌리면 앱의 닫은 시각이 지워지므로, outbox 행에 `at`을 담아 둔다.
- **`Arrive`가 먼저다.** levyan이 들은 적 없는 워크스페이스의 사건은 NotFound다. 같은 outbox에 차례로 쓰면 `Arrive`가 먼저 나간다. 한 워크스페이스의 생애 사건끼리는 순서가 바뀌어도 괜찮다.
- **다시 보내도 된다.** 같은 사건을 다시 말하면 아무것도 바뀌지 않고 성공으로 답한다. 이미 파기된 워크스페이스에 더 늦은 `at`으로 `Destroy`를 보내도 성공이고 바뀌는 것은 없다. 더 이른 `at`이면 그때가 파기한 시각이 된다.
- **늦게 보내도 된다.** 기간이 이미 닫힌 뒤에 그 기간의 사건이 오면(levyan이 멈췄던 동안 등), 그 기간은 다음 내역에서 정정된다. 답의 `late`가 그렇다고 말한다. 앱이 따로 할 것은 없다.
- **복원은 닫힘까지 거슬러 닿는다.** 되돌리면 닫혀 있던 구간을 청구하므로, 닫은 때가 든 기간이 이미 닫혔다면 복원을 제때 보내도 `late`가 참이다. 그 기간들이 다음 내역에서 정정된다(닫혀 있던 구간이 청구된다). 이것도 앱이 따로 할 것은 없다.

### 파기와 마지막 측정

1. 파기할 수 있는지 먼저 본다(법적 보존, 보관소). 파기하지 않는다면 아무것도 보내지 않는다. 무엇을 지울지 말하기만 하는 실행(dry run)도 보내지 않는다.
2. 그 워크스페이스의 모든 미터를 재서 `MeteringService/Record`로 넘긴다(design 7.3절의 마지막 측정, loft는 `closing.Destroy`의 `meter.Reader.Read`). 재지 못해도 파기는 하고, levyan은 마지막 일일 값을 파기 시각까지 매긴다.
3. 그 뒤의 시각을 `at`으로 `Destroy`의 outbox 행을 쓴다. levyan은 `at`과 그 뒤에 잰 값을 `REFUSAL_DESTROYED`로 거부한다. 그래서 `at`은 마지막 측정의 시각보다 뒤여야 한다. 거부된 값은 다른 거부처럼 로그에 남기고 다시 보내지 않는다.
4. 그리고 지운다. 중간에 멈춘 파기를 다시 하면서 다시 재면 그 값은 `REFUSAL_DESTROYED`다. levyan이 답한 것이므로 측정을 못 한 것이 아니다. 다시 하는 파기가 `Destroy`를 다시 보내도 된다.

outbox 행은 파기되는 워크스페이스의 행이 아니라 배포의 것이어야 한다. 파기가 그 워크스페이스의 행을 지울 때 함께 지워지면 `Destroy`가 나가지 않는다.

### 답과 오류

- 성공: 답의 `lifecycle`이 levyan이 지금 아는 그 워크스페이스다(`closed_since`, `destroyed`). outbox 행을 지운다.
- Unavailable, DeadlineExceeded, 응답 없음: 같은 요청을 그대로 다시 보낸다.
- NotFound: levyan이 들은 적 없는 워크스페이스다. 그 워크스페이스의 `Arrive`(만든 사람과 만든 때)를 먼저 보내고 다시 보낸다. levyan을 붙이기 전에 파기된 워크스페이스라면 보낼 것이 없다(행을 지운다).
- FailedPrecondition: 이미 파기된 워크스페이스에 대한 닫힘·복원이거나, 이미 다른 사건을 말한 시각에 다른 종류를 말한 것이다. 앱의 버그다. 로그에 남기고 행을 지운다.
- InvalidArgument: `at`이 없거나, 지금보다 5분 넘게 앞서거나(시계를 본다), 워크스페이스가 도착하기 전이다. 로그에 남기고 행을 지운다.
- `GetLifecycle(workspace)`로 levyan이 들은 것을 다시 읽는다. 들은 적 없는 워크스페이스는 열려 있다고 답한다.

### 처음 붙일 때 (지난 일)

이 호출을 처음 보내기 시작할 때, 그 전에 일어난 것을 한 번 보낸다. 그래야 levyan이 그 전부터 닫혀 있던 워크스페이스를 계속 청구하지 않는다.

- **닫힘과 복원**: 남아 있는 닫기·되돌리기 기록을 모두 오래된 것부터 보낸다(loft는 `workspace.close`, `workspace.restore` 이벤트의 시각). 기록이 없으면 적어도 지금 닫혀 있는 워크스페이스마다 `Close(at = 닫은 시각)`을 보낸다.
- **파기**: levyan이 아는 워크스페이스 중 이미 파기된 것마다 `Destroy(at = 파기한 시각)`을 보낸다(loft는 파기 때 쓰는 주소 보류 `AliasHold`의 워크스페이스와 시각). 보내지 않으면 그 워크스페이스의 지불자 연결이 끝나지 않아, 지불자의 무료 워크스페이스 한 자리를 계속 차지한다.
- 이미 닫힌 기간에 대한 것은 늦은 사건이라 다음 내역에서 정정된다.

### levyan이 하는 것

- 닫힌 동안에도 앱은 지금처럼 잰다(loft의 리더는 파기될 때까지 모든 워크스페이스를 잰다). levyan은 그 값을 남기고, 닫혀 있는 동안은 청구하지 않는다. 되돌리면 그 값으로 닫혀 있던 구간을 청구하고, 닫힌 채 파기되면 끝내 청구하지 않는다. 지불자는 그대로이고 Grant도 그대로다.
- 파기하면 지불자의 연결이 그 시각에 끝나고(`LINK_ENDED_DESTROYED`), 그 워크스페이스에 한 제안은 거둔다. 지불자 없음의 제한(`<제품>.restricted`)도 주지 않는다. 그 워크스페이스에 새 Grant를 주지 않고, 갱신되던 Grant는 갱신할 때가 되면 끝낸다(늦어도 3.5일 안). 끝이 정해진 Grant는 그 끝까지다. 어느 쪽이든 앱이 모르는 tenant의 Grant이므로 사본에서 아무것에도 쓰이지 않는다.
- 파기한 워크스페이스를 같은 식별자로 되살리면(loft의 `tenant import`) levyan은 그 워크스페이스의 값을 거부한다(`REFUSAL_DESTROYED`). 어떻게 알릴지는 아직 정하지 않았다(design 17장 104번).

## 6. 결제가 실패하면: 연체, 유예, 제한

청구서의 청구가 실패하면 levyan은 다시 시도하고, 유예가 끝나면 그 청구서가 청구한 워크스페이스를 제한하고, 결제되면 제한을 푼다. **levyan은 아무에게도 메일을 보내지 않는다.** 사람에게 알리는 것은 앱이다. levyan은 무엇을 언제 하는지 읽을 수 있게 하고(`Dunning`), 막는 것은 Grant로 한다(3장).

### levyan이 하는 것

- **거절의 종류마다 levyan 배포의 정책 표의 행을 따른다**(`dunning.kinds`). 아래는 기본값이고 대표가 정했다(2026-11-01, design 17장 3번). 배포는 값을 바꿀 수 있다. 시각은 모두 첫 실패에서 잰다.

  | `kind` | 다시 시도 | 유예 | 제한 | 읽기 전용 |
  | --- | --- | --- | --- | --- |
  | `DECLINE_KIND_TEMPORARY` 일시적(잔액 부족, 일시정지·연체인 구독) | 1, 3, 7일 뒤 | 14일 | 유예 뒤 | 60일 뒤 |
  | `DECLINE_KIND_REPLACE` 결제수단 교체 필요(만료, 구독 없음·해지, 지불자의 승인 대기) | 없음 | 14일 | 유예 뒤 | 60일 뒤 |
  | `DECLINE_KIND_FRAUD` 부정 의심 | 없음 | 없음 | 바로 | 60일 뒤 |
  | `DECLINE_KIND_UNSPECIFIED` 그 밖(이유를 말하지 않은 거절) | 일시적과 같다 | | | |

  유예 동안에는 아무것도 막지 않는다.

  행은 마지막 실패의 종류를 따른다. 재시도가 다른 종류로 실패하면 그 종류의 행으로 옮긴다.
- **결제 계정이 다시 수금할 수 있게 되면 일정과 상관없이 바로 다시 청구한다**(levyan이 1분마다 본다, `dunning.every`). 지불자가 새 결제창을 끝냈거나, 기다리던 결제를 승인했거나, Paddle이 밀린 결제를 받은 경우다. 교체 필요는 이것으로만 다시 청구한다.
- **제한되는 때**(`restricted_from`)부터 그 청구서의 모든 워크스페이스에 `<제품>.restricted` ON을 준다. 그 결제 계정이 그 청구서의 기간에 낸 워크스페이스다. 지불자가 없는 워크스페이스의 제한(4장)과 같은 키다. 둘 다 제한하면 둘 다 풀릴 때까지 제한된다.
- **읽기 전용이 되는 때**(`readonly_from`)부터 같은 워크스페이스에 `<제품>.readonly` ON도 준다. 제한의 두 번째 단계이고, 무엇을 받고 무엇을 거부하는지는 3장의 "제한의 두 단계"다.
- **회수 불가.** 다시 시도할 것이 남지 않고 유예가 끝나면 청구서는 `INVOICE_STATE_UNCOLLECTIBLE`이 된다. 그래도 받을 돈이고 제한은 이어진다. 그 뒤라도 청구가 성공하면 결제됨이 된다.
- **결제되면**(재시도, 다시 수금할 수 있게 되어 한 청구, 정정 청구, 무효) 1분 안에 제한과 읽기 전용의 Grant가 함께 끝난다. 앱의 사본에는 스트림으로 온다.

### 읽는 곳

- **지불자의 결제 화면**: `GetBillingAccount(id)`의 `dunning`(그 결제 계정의 이 제품 청구서 중 결제가 실패하고 아직 받을 것이 있는 것, 먼저 제한되는 것부터)과 `GetInvoice(id)`의 `dunning`.
- **워크스페이스의 배너**: 제품 키로 부르는 `GetPayer(workspace)`의 `payer.dunning`. 그 워크스페이스를 청구한 청구서 중 결제가 실패한 것이다. 전에 내던 결제 계정의 것도 들어 있다.
- **`Dunning`**: `kind`(마지막 거절의 종류), `failed`(첫 실패), `next_retry`(다음 청구. 지난 시각이면 곧 한다. 없으면 일정에 남은 것이 없고, 다시 수금할 수 있게 되어야 청구한다), `grace_ends`, `restricted_from`, `readonly_from`(읽기 전용이 되는 때).

### 무엇을 언제 보여 주는가

| 언제 | 누구에게 | 무엇을 |
| --- | --- | --- |
| `dunning`이 생겼을 때(첫 실패) | 지불자 | 결제가 실패했다. `kind`가 일시적·그 밖이면 "`next_retry`에 다시 시도합니다. `restricted_from`까지 결제되지 않으면 제한됩니다". 교체 필요면 **바로** "결제수단을 바꿔 주세요". `collection.state`가 `NO_SUBSCRIPTION`·`CANCELED`면 `StartCheckout`으로, `NEEDS_ACTION`이면 Paddle의 안내(카드사 앱의 승인)를 따르게 한다. 부정 의심이면 "결제가 거절되어 제한되었습니다. 다른 결제수단을 써 주세요" |
| 다시 시도가 실패할 때 | 지불자 | 같은 화면이 `next_retry`를 다음으로 옮겨 보여 준다 |
| `restricted_from` 전 | 지불자, 워크스페이스의 소유자 | "`restricted_from`부터 제한됩니다"라는 배너. 메일을 보낸다면 며칠 전에(앱이 정한다) |
| `restricted_from`부터 | 워크스페이스의 구성원 | "결제가 밀려 제한되었습니다". 막는 것은 `<제품>.restricted` Grant로 한다(3장). 결제하러 가는 길은 지불자와 소유자에게 보인다 |
| `readonly_from` 전 | 지불자, 워크스페이스의 소유자 | "`readonly_from`부터 읽기 전용이 됩니다"라는 배너. 메일을 보낸다면 며칠 전에(앱이 정한다) |
| `readonly_from`부터 | 워크스페이스의 구성원 | "결제가 밀려 읽기 전용입니다. 읽기, 내보내기, 결제, 닫기, 떠나기만 됩니다". 막는 것은 `<제품>.readonly` Grant로 한다(3장) |
| 회수 불가 | 지불자 | levyan은 더 시도하지 않는다. 결제수단을 바꾸면(새 결제창) 바로 청구한다 |
| `dunning`이 사라질 때 | 지불자, 소유자 | 결제되었다. 제한은 1분 안에 풀린다(Grant의 끝이 스트림으로 온다) |

- **levyan은 연체를 앱에 밀어 주지 않는다.** 앱은 화면과 배너를 그릴 때 읽는다. 메일을 보내려면 결제 계정이나 워크스페이스를 돌아가며 읽어야 한다. 더 나은 길은 아직 정하지 않았다(design 17장 109번).
- 막는 것은 **Grant가 있을 때만** 한다. `restricted_from`이나 `readonly_from`이 지났는데 Grant가 아직 오지 않았으면 배너만 보이고 쓰기는 막지 않는다. Grant는 levyan이 1분 안에 준다.
- 지불자 없는 워크스페이스(4장)도 같은 표의 행(`payerless`)을 따른다. `GetPayer`의 `payerless_since`, `restricted_from`, `readonly_from`이 그것이다.

## 7. 혜택과 크레딧

운영자가 워크스페이스나 결제 계정에 혜택을 준다(#43, `levyan-admin allowance`, `pool`, `credit`). 앱은 주지 않는다. 제품이 주는 API(추천 보상 등)는 나중이다. 앱은 levyan이 답하는 것을 그대로 보여 준다. 왜 주었는지(운영자의 이유)는 API에 없다. 문구는 앱이 정한다.

### levyan이 하는 것

- **추가 무료량**(`Allowance`): 기간마다 미터의 무료량을 더 준다. 예를 들어 "자산 무료량 +500, 2027년 4월까지"다. 워크스페이스의 것이라 지불자가 바뀌어도, 지불자의 무료 워크스페이스 N개 밖이어도 받는다. 기간의 일부에만 효력이 있으면 그 몫만큼 준다(1월 11일부터면 1월에 500 × 21/31). 구간 가격(보존 기간) 미터에는 주지 않는다.
- **사용량 크레딧**(`Pool`): 미터 단위의 묶음이다. 예를 들어 "자산·월 10,000"이다. 무료량(가격의 무료량 + 추가 무료량)을 넘는 청구량이 먼저 이 묶음에서 빠지고, 남은 것만 단가로 매긴다. 만료가 빠른 것부터 빼고, 늘 만료된다(기본 1년). 효력이 있는 기간이면 그 기간의 청구량을 통째로 덮는다(기간의 몫으로 나누지 않는다). 구간 가격 미터에는 주지 않는다.
- **금액 크레딧**(`Credit`): 청구서 합계에서 채널에 청구하기 전에 먼저 빼는 돈이다. 결제 계정의 것은 그 결제 계정의 청구서 전체에, 워크스페이스의 것은 그 워크스페이스의 줄에만 쓴다. 늘 만료되고(기본 1년), 만료가 빠른 것부터 쓰며, 만료된 것은 쓰지 않는다. 결제 계정의 잔액(돌려받을 돈, 만료 없음)은 그 뒤에 쓴다. 채널(Paddle)은 남은 금액만 본다. Paddle은 그 금액에 세금을 매기므로 할인과 같다.

### 읽는 곳

- **워크스페이스의 사용량**: 제품 키로 `BillingService.GetUsage(workspace)`.
  - `usage.allowances`: 지금 기간에 효력이 있는 추가 무료량. 미터(`meter`), 기간마다 더 주는 양(`quantity`), 시작(`from`)과 끝(`until`, 없으면 끝날 때까지).
  - 줄(`lines[]`)의 `allowance`: 이 기간에 추가 무료량이 준 것(효력이 있던 몫). `included`는 가격의 무료량과 추가 무료량을 합한 것이다. 그래서 `usage` − `included`가 `billable`이다(사용량이 무료량보다 작으면 0).
  - 줄의 `parts[]`: 가격 버전과 추가 무료량이 같던 구간마다 하나다. `included`는 가격의 무료량, `allowance`는 그 구간의 추가 무료량, `allowances`는 그 id다. `pooled`는 그 구간의 청구량에서 사용량 크레딧이 뺀 몫이다.
  - `usage.pools`: 이 기간이 쓸 수 있는 사용량 크레딧이다. 미터, 준 양(`granted`), 닫힌 기간들이 쓰고 남은 양(`left`), 시작(`from`)과 만료(`expires`)가 있다.
  - 줄의 `pooled`와 `pools[]`(`pool`, `taken`): 이 기간의 청구량에서 사용량 크레딧이 빼는 양이다. 단가를 곱하는 것은 `billable` − `pooled`다. 이번 기간이 끝나면 남을 양은 `left` − (이 기간의 `taken`)이다.
  - 닫은 기간의 내역(`ListStatements`, `GetStatement`)의 줄도 같다.
- **워크스페이스의 크레딧**: 제품 키로 `WorkspaceService.GetPayer(workspace)`의 `payer.credits`. 그 워크스페이스에 준 크레딧 중 남은 것이다. 남은 금액(`left`), 준 금액(`granted`), 통화, 만료(`expires`)가 있다.
- **지불자의 결제 화면**: 대신 부르는 `PayerService.GetBillingAccount(id)`.
  - `credits`: 그 결제 계정의 다음 청구서가 쓸 수 있는 크레딧이다. 그 결제 계정의 것과, 그 결제 계정이 지금(또는 다음 기간부터) 내는 워크스페이스의 것이고, 만료가 빠른 것부터다. 워크스페이스의 것은 `workspace`가, 결제 계정의 것은 `billing_account`가 있다.
  - `balance`: 그 제품에서 그 결제 계정이 돌려받을 잔액이다(음수 청구서, 정정 청구, 더 낸 돈). 만료가 없고, 크레딧 뒤에 쓴다.
- **청구서**: `PayerService.GetInvoice(id)`의 `invoice.credited`(크레딧이 낸 합계)와 `invoice.credits`(무엇이 얼마를 냈는지). 크레딧마다 하나이고(`credit`, `workspace`, `expires`), 잔액이 낸 것은 `credit`이 비어 있다. 채널에 청구한 것은 `due`다.
- 금액은 모두 통화의 최소 단위 정수다(원, 센트). 사용량과 무료량은 소수점 아래 6자리까지의 10진수 문자열이다.

### 무엇을 언제 보여 주는가

| 언제 | 어디서 | 누구에게 | 무엇을 |
| --- | --- | --- | --- |
| 추가 무료량이 있을 때 | 사용량 화면 | 워크스페이스의 소유자, 지불자 | "프로모션 무료량 +500 (자산, 2027년 4월 30일까지)". 줄마다 "무료 100 + 프로모션 338.7". `allowance`가 0이 아닌 줄에 표시한다 |
| 사용량 크레딧이 있을 때 | 사용량 화면 | 워크스페이스의 소유자, 지불자 | "사용량 크레딧 자산·월 10,000 중 9,300 남음 (2027년 11월 1일 만료)". 줄마다 "청구 대상 700 중 크레딧에서 700". 남은 양이 이번 기간에 다 쓰일 것 같으면 알린다 |
| 금액 크레딧이 있을 때 | 결제 화면 | 지불자 | "크레딧 70,000원 남음 (2027년 11월 1일 만료)". 워크스페이스의 것은 그 워크스페이스의 라벨과 함께. 잔액은 "만료 없음" |
| 만료가 가까울 때(앱이 정한다, 예: 30일 전) | 결제 화면, 배너 | 지불자 | "크레딧 30,000원이 곧 만료됩니다". levyan은 알리지 않는다. 앱이 `credits[].expires`로 판단한다 |
| 청구서를 볼 때 | 청구서 | 지불자 | 합계, "크레딧 −50,000원"(`credits[]`마다, 잔액은 따로), 청구 금액(`due`) |
| 워크스페이스 관리 화면 | 워크스페이스 | 소유자 | 그 워크스페이스에 준 크레딧(`GetPayer`의 `payer.credits`) |

- **levyan은 혜택을 앱에 밀어 주지 않는다.** 화면을 그릴 때 읽는다. 연체(6장)와 같다.
- 사용량 화면의 예상 금액(`GetUsage`의 `total`)은 추가 무료량과 사용량 크레딧을 뺀 뒤이고, 금액 크레딧을 빼기 전이다. 금액 크레딧은 청구서를 확정할 때 쓰기 때문이다. 크레딧이 있으면 "크레딧에서 먼저 냅니다"를 함께 보인다.
- 늦게 온 값이 지난 기간을 정정하면, 그 기간이 사용량 크레딧에서 쓴 양도 정정 줄에 다시 적힌다(그 기간이 지금 쓰는 양). 금액이 그대로여도 정정 줄이 있을 수 있다.
