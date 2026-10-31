# Kubernetes에 levyan 배포하기 (운영자)

이 디렉터리는 levyan의 **참조 배포**입니다. 매니페스트를 kustomize base로 두고, 실제 배포는 그 위에 얹는 **오버레이**로 합니다. loft의 `deploy/k8s`와 같은 방식입니다.

```yaml
resources:
  - github.com/lesomnus/levyan//deploy/k8s?ref=<커밋>   # 브랜치가 아니라 커밋으로 고정합니다
```

## 무엇이 있나

| | |
| --- | --- |
| `levyan` Deployment | 파드 하나에 컨테이너 셋이 있습니다. `migrate`(init 컨테이너, `levyan migrate`), `levyan`(`levyan serve`, gRPC 50051), `levyan-admin`(운영자 진입점, 8081)입니다 |
| `levyan` Service | 50051 하나입니다. 제품이 키를 들고 부르는 곳이고, 흐름 미터의 OTLP도 같은 포트로 받습니다 |
| NetworkPolicy `levyan` | 네트워크로는 50051만 받습니다. 운영자 진입점에는 네트워크로 아무것도 닿지 않습니다 |
| ConfigMap `levyan` | `config.yaml`입니다. 모든 프로세스가 `--config`로 읽습니다 |

볼륨은 없습니다. levyan이 지키는 것은 모두 DB에 있습니다. 감사 기록의 보관소(`audit.archive`)를 두려면 오버레이가 볼륨을 붙입니다.

**Secret도 없습니다.** 오버레이가 같은 이름으로 만들어 넣습니다. base에 Secret이 있으면 오버레이의 것과 이름이 부딪혀 빌드가 되지 않습니다.

파드는 `restricted` Pod Security를 통과합니다. root가 아닌 사용자(65532)로 돌고, 루트 파일 시스템은 읽기 전용이며, 권한은 모두 내려놓고, 서비스 계정 토큰도 받지 않습니다.

## 오버레이가 가져올 것

1. **namespace.** `pod-security.kubernetes.io/enforce: restricted` 라벨을 권합니다.
2. **Secret 셋.**

   | Secret | 키 | 내용 | 만드는 법 |
   | --- | --- | --- | --- |
   | `levyan` | `db-dsn` | levyan의 DB 주소. 예: `postgres://levyan:…@db:5432/levyan?sslmode=require` | |
   | `levyan-keys` | `digest.key` | 감사 기록이 행마다 남기는 지문(HMAC)의 키. `levyan trail rows`가 이것으로 DB에서 직접 바뀐 행을 찾습니다. 잃으면 이전 지문을 검증할 수 없으니 **백업에 함께 넣되 DB 백업과는 따로** 둡니다 | `openssl rand -base64 32` |
   | `levyan-admin` | 운영자 이름마다 파일 하나(`operator`) | 운영자 키, 32자 이상. 운영자를 더하면 `admin.keys`에도 이름을 더합니다 | `openssl rand -hex 32` |

3. **설정.** base의 `config.yaml`을 복사해 채우고 바꿔 끼웁니다.

   ```yaml
   configMapGenerator:
     - name: levyan
       behavior: replace
       files:
         - levyan.yaml=config.yaml
   ```

   채울 것은 텔레메트리(`otel:`)와, 정해지면 감사 기록의 보존 기간(`audit.retain`, `audit.by`)입니다. 보존 기간이 없으면 감사 기록은 지워지지 않습니다. 돈의 기록이니 배포가 보존을 정하기 전까지는 그게 맞습니다.

4. **NetworkPolicy 좁히기.** `from`을 levyan을 부르는 제품의 파드로 좁힙니다. 키가 제품을 알아보지만, 네트워크가 그 앞의 두 번째 벽입니다.

   ```yaml
   patches:
     - target: { kind: NetworkPolicy, name: levyan }
       patch: |-
         - op: add
           path: /spec/ingress/0/from
           value:
             - namespaceSelector:
                 matchLabels: { kubernetes.io/metadata.name: loft }
               podSelector:
                 matchLabels: { app.kubernetes.io/name: loft }
   ```

5. **이미지 고정.** `images:`에서 digest로 고정합니다. CI가 main에서 올린 `ghcr.io/lesomnus/levyan:edge`의 digest와 그 revision 라벨을 확인하고 씁니다.

## 운영자 진입점

운영자 진입점은 네트워크로 닿지 않습니다. 파드 안에서 부릅니다.

```sh
kubectl exec deploy/levyan -c levyan-admin -- \
  levyan-admin --key '${file:/run/secrets/levyan-admin/operator}' product ls
```

또는 `kubectl port-forward deploy/levyan 8081`로 열고, 자기 자리에서 `levyan-admin --url http://127.0.0.1:8081 --key …`로 부릅니다.

처음 할 일은 이렇습니다.
1. 제품을 만듭니다: `product add loft --name Loft --yes`
2. 시간대·채널·통화를 정합니다: `product configure loft --zone Asia/Seoul --channel mor --currency KRW --yes` (국내 결제 계정의 기본인 MoR과 원. 가격도 그 채널과 통화로 둡니다)
3. 키를 발급합니다: `key issue loft --alias … --yes`. 키는 마지막 줄에 **한 번만** 나옵니다. 바로 제품 쪽 Secret에 넣습니다.

## 새 버전

이미지의 digest를 바꾸고 `kubectl apply -k`합니다. `Recreate`이므로 이전 파드가 내려간 뒤에 새 파드의 init 컨테이너가 마이그레이션합니다. 그 사이 몇 초 끊깁니다. 제품은 다시 보내는 것을 전제로 만들어져 있습니다(계량 값은 멱등입니다).

바뀐 Secret 파일이 있으면 **`apply -k`보다 먼저** 적용합니다. 새 설정이 아직 없는 파일을 가리키면 파드가 뜨지 않습니다.
