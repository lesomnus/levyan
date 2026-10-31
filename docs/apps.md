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

- **제한은 있으면 막는 Grant로 검사한다.** levyan은 제한을 `loft.restricted` ON 같은 Grant로 준다(지불자 없이 유예가 지난 워크스페이스는 #4, 연체는 #11). 앱은 그 Grant가 **있을 때** 막는다.

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

### levyan이 멈췄을 때

- 사본은 마지막으로 들은 대로 답한다. 소식을 못 들었다고 지우지 않는다.
- Grant는 저마다 끝(`valid_until`)이 있다. 갱신되는 Grant의 끝은 마지막 갱신에서 7일(`grants.horizon`)이고, 반이 남으면 갱신된다. 그래서 levyan이 멈춰도 사본은 적어도 3.5일 동안 Grant를 지킨다.
- 끝이 지난 Grant를 어떻게 할지는 앱의 정책이다. 하루 더 봐주려면 `entitle.All`과 `entitle.Decide`로 읽는다(payday 가이드 5장).

### 키

키는 앱의 이름으로 시작하는 점으로 이은 소문자 단어다(`entitle.CheckKey`): `loft.export`, `loft.seats`, `loft.restricted`. 어떤 키에 무엇을 주는지는 levyan이 정하고, 앱은 검사할 키만 안다. 지금 levyan이 주는 것은 운영자의 Grant(파일럿, 계약, 체험)와, 지불자 없이 유예가 지난 워크스페이스의 `loft.restricted`(4장)다. 연체의 제한은 #11이 더한다.

## 4. 누가 내는가: 워크스페이스의 지불자

지불자는 roster 계정이다. 워크스페이스마다 한 번에 한 명이고, 결제 계정으로 낸다. 앱은 levyan에 두 가지를 알린다.

### 앱이 알리는 것 (`levyan.v1.WorkspaceService`, 제품 키로)

- **`Arrive(workspace, creator, created)`**: 워크스페이스를 만들 때마다 부른다. 만든 사람이 만든 때부터 지불자다. 다시 불러도 같으므로, 실패하면 그대로 다시 보낸다. levyan을 붙이기 전에 있던 워크스페이스는 한 번씩 `created`에 만든 때를 넣어 부른다.
- **`Leave(workspace, account)`**: 계정이 워크스페이스를 떠날 때 부른다(roster가 계정을 지운 경우 포함). 그 계정이 지불자였다면 지금부터 지불자가 없다.
- **`Offer(workspace, account)`**: 소유자가 다른 계정에게 지불자가 되어 달라고 할 때 부른다. 받아들이는 것은 그 계정이 한다(아래).
- **`GetPayer(workspace)`**: 지금과 다음 기간의 지불자, 열린 제안, 지불자가 없는 때(`payerless_since`)와 제한되는 때(`restricted_from`). 소유자에게 "지불자가 없습니다, 언제부터 제한됩니다"를 보여 주는 것은 앱이다.

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
3. **결제 화면**: `GetBillingAccount(id)`의 `collection`(수금할 수 있는지와 이유)과 `subscription`(상태, 다음 청구, 예정된 해지, 결제수단의 종류·브랜드·끝 네 자리)을 그린다. `collection.state`가
   - `COLLECTABLE`: 정상. `ends`가 있으면 그때 해지되거나 일시정지된다(지불자가 Paddle에서 예약했다).
   - `NO_SUBSCRIPTION`: 결제수단을 등록하게 한다(`StartCheckout`).
   - `PAST_DUE`: 결제가 실패해 Paddle이 다시 시도하는 중이다.
   - `NEEDS_ACTION`: 결제가 지불자를 기다린다(국내 카드의 앱 승인, 은행의 확인). Paddle이 보낸 안내를 따르게 한다.
   - `PAUSED`, `CANCELED`: 구독이 멈췄거나 끝났다. 끝났으면 새로 `StartCheckout`할 수 있다.
4. **워크스페이스의 배너**: 제품 키로 부르는 `GetPayer(workspace)`의 `payer.collection`이 지금 내는 결제 계정의 상태다. 결제수단은 보이지 않는다. `COLLECTABLE`이 아니면 소유자에게 알린다. 그 뒤에 무엇을 막을지는 levyan이 Grant로 정한다(#11, 3장).

### 지불자가 없으면

마지막 지불자가 떠난 지 30일(`payers.grace`)이 지나면 levyan이 `<제품>.restricted` ON을 준다(loft는 `loft.restricted`). 앱은 3장처럼 **그 Grant가 있을 때** 막는다. 누가 받아들이면 Grant는 끝난다.
