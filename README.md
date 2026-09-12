# champions

Google Cloud Project払い出し機

ハンズオンの参加者に、その場で Google Cloud Project を払い出すためのアプリケーション。
参加者はイベントコードを入力するだけで、自分の Google Account に権限が付いた Project を受け取れる。

## 仕様

アプリケーションは一般公開用と Admin 用の 2 つがあり、`cmd/server` と `cmd/admin` に main.go を置いている。
どちらも Cloud Run に Deploy し、Deploy は Cloud Build で行う。

### 一般公開アプリケーション機能

IAP で認証があり、認証を行った Google Account に Google Cloud Project を払い出す。
ハンズオン用イベントコードを入力すると Google Cloud Project を作成し、認証を行ったユーザにイベントとして設定されている権限を付与する。
ProjectID はイベントコード + `-` + 4 文字のランダムな英数字。1 つのイベントに参加するのは多くても 100 人未満。
Firestore にどのユーザにどの Project を払い出したのかを保存する。
API の Enable や Quota はイベントの設定に合わせて行い、これらは Cloud Tasks で非同期に実行する。

### Admin 機能

IAP で認証があり、ハンズオン用イベントコードを管理する。
任意の文字列のイベントコードを入力すると Firestore にイベントを登録する。
イベントごとに払い出す Google Cloud Project は同じフォルダーに入れるため、イベントコードと同じ名前のフォルダを作成する。
イベント用のフォルダは `FOLDER_PARENT` の直下に作った `champions` フォルダの中にまとめる。
イベントごとに、対象のユーザに付与する IAM、Enable にする API、Quota の設定を編集できる。
ハンズオンが終わったら、イベントに紐づく Project をまとめて削除依頼状態にできる。

## 構成

```
参加者 ──IAP──▶ champions-server (Cloud Run)
                     │  Firestore に Allocation を PENDING で作成
                     ▼
                Cloud Tasks (champions-provision)
                     │  OIDC トークン付きで POST /tasks/provision
                     ▼
                champions-worker (Cloud Run / server と同じイメージ)
                     │
                     ├─ Project 作成 (イベントのフォルダ配下)
                     ├─ 請求先アカウントの紐付け
                     ├─ 参加者への IAM Role 付与
                     ├─ API の Enable
                     └─ Cloud Quotas の設定

運営 ──IAP──▶ champions-admin (Cloud Run)
                     ├─ イベントの登録 / 編集 (Firestore)
                     ├─ イベント用フォルダの作成
                     ├─ 払い出し状況の確認
                     └─ Shutdown ──▶ Cloud Tasks (champions-shutdown)
                                          └──▶ champions-worker
                                                 └─ Project を 1 件ずつ削除依頼
```

フォルダは 2 階層で作る。`FOLDER_PARENT` の直下に `champions` フォルダを 1 つ作り、
イベント用のフォルダはその中に作る。どちらも同名のフォルダがあれば再利用する。

```
FOLDER_PARENT (organizations/123 もしくは folders/456)
└── champions                     ← ROOT_FOLDER_NAME
    ├── go-handson-2026           ← イベントコードと同じ名前
    │   ├── go-handson-2026-a3k9  ← 参加者に払い出した Project
    │   └── go-handson-2026-x7m2
    └── infra-handson
        └── infra-handson-p4q8
```

Project の作成は 30 秒程度かかる LRO なので、リクエスト内では Firestore にレコードを作るだけにして、
実際の払い出しはすべて Cloud Tasks の worker 側で行う。画面はポーリングで進捗を表示する。

ハンズオン後の Project の片付けも同じ worker が受け持つが、キューは分けている。
払い出しは参加者を待たせないよう並列に捌きたいのに対し、片付けは急がず 1 件ずつ順番に消したいためで、
リトライ間隔も別々にしたい。worker の service account は同じなので IAM の設定は共通でよい。

worker は `cmd/server` と同じバイナリで、`/tasks/` 配下だけを Cloud Tasks の OIDC トークンで認証している。
IAP を付けた `champions-server` とは別サービスとして Deploy することで、Cloud Tasks が IAP に阻まれずに worker を呼べる。

### ディレクトリ

| パス | 内容 |
| --- | --- |
| `cmd/server` | 一般公開アプリケーション + 払い出し worker |
| `cmd/admin` | Admin アプリケーション |
| `internal/config` | 環境変数からの設定読み込み |
| `internal/iap` | IAP の JWT 検証 |
| `internal/tasks` | Cloud Tasks への投入と OIDC トークンの検証 |
| `internal/model` | Firestore のエンティティと検証 |
| `internal/store` | Firestore の読み書き |
| `internal/gcp` | Resource Manager / Service Usage / Cloud Quotas / Billing の操作 |
| `internal/provision` | 払い出し処理の本体 |
| `internal/shutdown` | ハンズオン後の Project の片付け |
| `internal/server` | 一般公開アプリケーションの HTTP ハンドラと画面 |
| `internal/admin` | Admin アプリケーションの HTTP ハンドラと画面 |

## データモデル

### Firestore `Events` (Document ID = イベントコード)

| フィールド | 内容 |
| --- | --- |
| `Code` | イベントコード。英小文字・数字・ハイフンの 2〜25 文字 |
| `DisplayName` | 画面表示用のイベント名 |
| `FolderName` | 払い出した Project を入れるフォルダ。`folders/1234567890` |
| `Enabled` | false の間は払い出しを受け付けない |
| `MaxAllocations` | 払い出せる Project 数の上限。0 は無制限 |
| `Roles` | 参加者に付与する IAM Role。`roles/owner` など |
| `APIs` | Enable にするサービス。`compute.googleapis.com` など |
| `Quotas` | 適用する Quota (Service / QuotaID / Dimensions / PreferredValue / ContactEmail) |

イベントコードは ProjectID の接頭辞になるため、`{イベントコード}-{4文字}` が ProjectID の上限 30 文字に収まるよう
2〜25 文字に制限している。

### Firestore `Allocations` (Document ID = `{イベントコード}:{email}`)

Document ID を決定的にすることで、1 ユーザが 1 イベントで 2 つ Project を受け取ることを Firestore の Create で防いでいる。

| フィールド | 内容 |
| --- | --- |
| `EventCode` / `UserEmail` / `UserID` | 誰がどのイベントで受け取ったか |
| `ProjectID` / `ProjectName` / `FolderName` | 払い出した Project |
| `Status` | `PENDING` → `PROVISIONING` → `READY` / `FAILED`。後片付け後は `SHUTDOWN` |
| `Step` | 進捗。`CREATE_PROJECT` / `GRANT_IAM` / `ENABLE_SERVICES` / `APPLY_QUOTAS` など |
| `Attempts` / `Error` | worker の試行回数と直近のエラー |
| `ExpireAt` | このレコードの削除予定時刻。`CreatedAt` の 30 日後 |

`Allocations` は Firestore の [TTL ポリシー](https://cloud.google.com/firestore/native/docs/ttl) で
`ExpireAt` を過ぎたものが自動的に消える。保持期間は `model.AllocationTTL` で 30 日にしている。
削除されるのは Firestore のレコードだけで、払い出した Project には影響しない。

TTL の削除は期限から 24 時間以内をめどに行われるので、ちょうど 30 日で消えるわけではない。

払い出し処理は Cloud Tasks のリトライ前提で、どのステップも冪等になるように書いてある。
`MAX_PROVISION_ATTEMPTS` (既定 5) を超えると `FAILED` にしてリトライを打ち切る。

## API

### 一般公開アプリケーション

| メソッド | パス | 内容 |
| --- | --- | --- |
| `GET` | `/api/me` | 認証されている email |
| `GET` | `/api/allocations` | 自分が受け取った払い出しの一覧 |
| `POST` | `/api/allocations` | `{"eventCode": "..."}` で払い出しを申し込む |
| `GET` | `/api/allocations/{id}` | 払い出しの状態 |
| `POST` | `/tasks/provision` | Cloud Tasks から呼ばれる払い出し worker |
| `POST` | `/tasks/shutdown` | Cloud Tasks から呼ばれる後片付け worker |

### Admin

| メソッド | パス | 内容 |
| --- | --- | --- |
| `GET` | `/api/events` | イベント一覧 |
| `POST` | `/api/events` | イベントを登録し、同名のフォルダを作る |
| `GET` `PUT` `DELETE` | `/api/events/{code}` | イベントの取得 / 更新 / 削除 |
| `POST` | `/api/events/{code}/folder` | フォルダの作成をやり直す |
| `GET` | `/api/events/{code}/allocations` | そのイベントの払い出し状況 |
| `POST` | `/api/events/{code}/shutdown` | 払い出した Project の片付けを Cloud Tasks に投入する |

`DELETE /api/events/{code}` はイベントの設定だけを消す。払い出し済みの Project とフォルダは残る。

`POST /api/events/{code}/shutdown` はハンズオン後の後片付けに使う。
削除中に新しい払い出しが走らないよう先にイベントの受付を止め、片付け自体は Cloud Tasks に渡して 202 を返す。
worker は Project を 1 件ずつ順番に削除依頼状態にし、成功した Allocation をその都度 `SHUTDOWN` に更新する。
そのため途中で失敗して Cloud Tasks にリトライされても、済んでいる分は飛ばして続きから再開する。
既に削除済みの Project や ProjectID が採番される前に失敗した払い出しはスキップし、
1 件失敗しても残りは続けてから error を返す。Project は 30 日間は復元できる。
進捗は Allocation の `Status` に出るので、Admin の画面はそれをポーリングして表示する。
イベントの設定とフォルダは残るため、同じイベントコードで払い出しを再開できる。

## 環境変数

| 変数 | 必須 | 内容 |
| --- | --- | --- |
| `GOOGLE_CLOUD_PROJECT` | ○ | champions 自身が動く Project。Firestore と Cloud Tasks の所属先 |
| `FIRESTORE_DATABASE_ID` | | 使う Firestore のデータベース ID。既定 `(default)` |
| `PORT` | | 待ち受けポート。既定 8080 (Cloud Run が設定する) |
| `IAP_AUDIENCE` | | IAP が発行する JWT の `aud`。未設定でも起動はするが、リクエストはすべて 401 になる |
| `DEV_USER_EMAIL` | | 指定すると IAP の検証を行わず、この email のユーザとして動く。ローカル開発専用 |
| `FOLDER_PARENT` | admin で ○ | `champions` フォルダを作る親。`organizations/123` もしくは `folders/456` |
| `ROOT_FOLDER_NAME` | | `FOLDER_PARENT` の下に作るルートフォルダ名。既定 `champions` |
| `BILLING_ACCOUNT` | | `billingAccounts/XXXXXX-XXXXXX-XXXXXX`。ID だけでもよい。空なら請求先の紐付けを行わない。Deploy 時は Secret Manager から入る |
| `ADMIN_EMAILS` | | Admin を使える email のカンマ区切り。空なら IAP の許可のみで判定 |
| `TASKS_LOCATION` | ○ | Cloud Tasks キューのロケーション |
| `TASKS_PROVISION_QUEUE` | | 払い出しを積むキュー名。既定 `champions-provision` |
| `TASKS_SHUTDOWN_QUEUE` | | 後片付けを積むキュー名。既定 `champions-shutdown` |
| `WORKER_BASE_URL` | ○ | worker の URL。OIDC トークンの audience にもなる |
| `WORKER_INVOKER_SERVICE_ACCOUNT` | ○ | Cloud Tasks が OIDC トークンを発行する service account |
| `TASK_INVOKER_EMAILS` | | `/tasks/` を呼べる service account。空なら `WORKER_INVOKER_SERVICE_ACCOUNT` のみ |
| `MAX_PROVISION_ATTEMPTS` | | リトライ上限。既定 5 |
| `LOCAL_TASKS` | | `true` にすると Cloud Tasks を使わず、アプリケーション内で払い出しと片付けを実行する |

## セットアップ

以下は `champions` を動かす Project を `${PROJECT_ID}`、組織を `${ORG_ID}`、リージョンを `asia-northeast1` とした例。

### 1. API の有効化と Firestore / Cloud Tasks

```sh
gcloud services enable \
  run.googleapis.com \
  cloudbuild.googleapis.com \
  artifactregistry.googleapis.com \
  firestore.googleapis.com \
  cloudtasks.googleapis.com \
  cloudresourcemanager.googleapis.com \
  serviceusage.googleapis.com \
  cloudquotas.googleapis.com \
  cloudbilling.googleapis.com \
  secretmanager.googleapis.com \
  iap.googleapis.com \
  --project=${PROJECT_ID}

# 既定のデータベース ((default)) を作る。名前付きのデータベースを使う場合は
# --database=NAME を付けて作り、FIRESTORE_DATABASE_ID に同じ名前を設定する。
gcloud firestore databases create --location=asia-northeast1 --project=${PROJECT_ID}

gcloud tasks queues create champions-provision \
  --location=asia-northeast1 \
  --max-concurrent-dispatches=10 \
  --max-attempts=5 \
  --min-backoff=10s \
  --max-backoff=300s \
  --project=${PROJECT_ID}

gcloud artifacts repositories create champions \
  --repository-format=docker --location=asia-northeast1 --project=${PROJECT_ID}
```

Firestore は `Allocations` を `EventCode` / `UserEmail` で絞って `CreatedAt` 順に読むため、複合インデックスが必要になる。
初回アクセス時にエラーメッセージに出るリンクから作るか、次のコマンドで作る。

名前付きのデータベースを使う場合は `--database` にその名前を渡す。既定のデータベースなら省略できる。

```sh
gcloud firestore indexes composite create \
  --collection-group=Allocations --field-config=field-path=EventCode,order=ascending \
  --field-config=field-path=CreatedAt,order=descending --project=${PROJECT_ID}

gcloud firestore indexes composite create \
  --collection-group=Allocations --field-config=field-path=UserEmail,order=ascending \
  --field-config=field-path=CreatedAt,order=descending --project=${PROJECT_ID}
```

`Allocations` は作成から 30 日で消えるように TTL ポリシーを設定する。

```sh
gcloud firestore fields ttls update ExpireAt \
  --collection-group=Allocations --enable-ttl --project=${PROJECT_ID}
```

TTL のフィールドにインデックスが張られているとホットスポットになりやすいので、Standard edition では除外しておく。
Enterprise edition は単一フィールドの自動インデックスがそもそも無効なので、この指定は不要。

```sh
gcloud firestore indexes fields update ExpireAt \
  --collection-group=Allocations --disable-indexes --project=${PROJECT_ID}
```

### 2. 請求先アカウントのシークレット

請求先アカウントはビルド設定に直接書かず、Secret Manager に入れて Cloud Build から取り出す。

```sh
printf 'billingAccounts/%s' ${BILLING_ACCOUNT_ID} | \
  gcloud secrets create champions-billing-account --data-file=- --project=${PROJECT_ID}
```

値は `billingAccounts/` を付けても付けなくてもよい。付いていなければアプリケーション側で補う。

読み取りの許可は [3. Service Account](#3-service-account) で `champions-build` を作った後に行う。

```sh
gcloud secrets add-iam-policy-binding champions-billing-account \
  --member=serviceAccount:champions-build@${PROJECT_ID}.iam.gserviceaccount.com \
  --role=roles/secretmanager.secretAccessor --project=${PROJECT_ID}
```

シークレット名を変えたい場合は `_BILLING_ACCOUNT_SECRET` で指定する。
請求先を紐付けたくない場合は、空文字を入れたシークレットを作っておく。

### 3. Service Account

Cloud Build 既定の service account は使わず、用途ごとに 2 つ作る。

| service account | 用途 |
| --- | --- |
| `champions-app` | Cloud Run (server / worker / admin) の実行。Cloud Tasks が worker を呼ぶ OIDC トークンの発行元も兼ねる |
| `champions-build` | Cloud Build でのビルドと Deploy |

```sh
gcloud iam service-accounts create champions-app \
  --display-name="champions Cloud Run runtime" --project=${PROJECT_ID}
gcloud iam service-accounts create champions-build \
  --display-name="champions Cloud Build" --project=${PROJECT_ID}

APP_SA=champions-app@${PROJECT_ID}.iam.gserviceaccount.com
BUILD_SA=champions-build@${PROJECT_ID}.iam.gserviceaccount.com
```

#### champions-app

| スコープ | Role | 用途 |
| --- | --- | --- |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/resourcemanager.folderCreator` | `champions` フォルダとイベント用フォルダの作成 |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/resourcemanager.folderViewer` | 既存フォルダの検索 |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/resourcemanager.projectCreator` | Project の作成 |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/resourcemanager.projectDeleter` | ハンズオン後の Project の削除 |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/resourcemanager.projectIamAdmin` | 参加者への Role 付与 |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/serviceusage.serviceUsageAdmin` | API の Enable |
| 組織 (もしくは `FOLDER_PARENT`) | `roles/cloudquotas.admin` | Quota の設定 |
| 請求先アカウント | `roles/billing.user` | Project への請求先の紐付け |
| `${PROJECT_ID}` | `roles/datastore.user` | Firestore |
| `${PROJECT_ID}` | `roles/cloudtasks.enqueuer` | Cloud Tasks への投入 |
| `champions-app` 自身 | `roles/iam.serviceAccountUser` | Cloud Tasks の OIDC トークン発行 |

```sh
for ROLE in roles/resourcemanager.folderCreator roles/resourcemanager.folderViewer \
            roles/resourcemanager.projectCreator roles/resourcemanager.projectDeleter \
            roles/resourcemanager.projectIamAdmin roles/serviceusage.serviceUsageAdmin \
            roles/cloudquotas.admin; do
  gcloud organizations add-iam-policy-binding ${ORG_ID} \
    --member=serviceAccount:${APP_SA} --role=${ROLE}
done

gcloud billing accounts add-iam-policy-binding ${BILLING_ACCOUNT_ID} \
  --member=serviceAccount:${APP_SA} --role=roles/billing.user

for ROLE in roles/datastore.user roles/cloudtasks.enqueuer; do
  gcloud projects add-iam-policy-binding ${PROJECT_ID} \
    --member=serviceAccount:${APP_SA} --role=${ROLE}
done

# Cloud Tasks に OIDC トークン付きのタスクを積むため、自分自身に対する actAs が要る。
gcloud iam service-accounts add-iam-policy-binding ${APP_SA} \
  --member=serviceAccount:${APP_SA} --role=roles/iam.serviceAccountUser --project=${PROJECT_ID}
```

Cloud Run の worker を呼べるようにする `roles/run.invoker` は、サービスができた後に付ける ([4. Deploy](#4-deploy))。

#### champions-build

| スコープ | Role | 用途 |
| --- | --- | --- |
| `${PROJECT_ID}` | `roles/logging.logWriter` | ビルドログの書き込み |
| `${PROJECT_ID}` | `roles/storage.objectViewer` | `gcloud builds submit` が上げたソースの読み取り |
| `${PROJECT_ID}` | `roles/artifactregistry.writer` | イメージの push |
| `${PROJECT_ID}` | `roles/run.admin` | Cloud Run への Deploy |
| `champions-app` | `roles/iam.serviceAccountUser` | Cloud Run を `champions-app` で動かすための actAs |
| シークレット `champions-billing-account` | `roles/secretmanager.secretAccessor` | 請求先アカウントの読み取り ([2. 請求先アカウントのシークレット](#2-請求先アカウントのシークレット)) |

```sh
for ROLE in roles/logging.logWriter roles/storage.objectViewer \
            roles/artifactregistry.writer roles/run.admin; do
  gcloud projects add-iam-policy-binding ${PROJECT_ID} \
    --member=serviceAccount:${BUILD_SA} --role=${ROLE}
done

gcloud iam service-accounts add-iam-policy-binding ${APP_SA} \
  --member=serviceAccount:${BUILD_SA} --role=roles/iam.serviceAccountUser --project=${PROJECT_ID}
```

service account の名前を変えたい場合は、cloudbuild.yaml の `_BUILD_SERVICE_ACCOUNT` と `_APP_SERVICE_ACCOUNT` で指定する。

### 4. Deploy

```sh
gcloud builds submit --config cloudbuild.yaml --project=${PROJECT_ID} \
  --substitutions=_REGION=asia-northeast1,_FOLDER_PARENT=organizations/${ORG_ID},_ADMIN_EMAILS=you@example.com
```

`champions-server` / `champions-worker` / `champions-admin` の 3 つの Cloud Run サービスができる。
`champions-worker` は `champions-server` と同じイメージで、Cloud Tasks からの呼び出しだけを受ける。

Deploy 後、Cloud Tasks が worker を呼べるように invoker を付ける。

```sh
gcloud run services add-iam-policy-binding champions-worker \
  --region=asia-northeast1 --project=${PROJECT_ID} \
  --member=serviceAccount:${APP_SA} --role=roles/run.invoker
```

### 5. IAP

`champions-server` と `champions-admin` は `--iap` 付きで Deploy されるので、IAP のアクセス権を設定する。

```sh
# 参加者 (ドメイン全体に開ける例)
gcloud run services add-iam-policy-binding champions-server \
  --region=asia-northeast1 --project=${PROJECT_ID} \
  --member=domain:example.com --role=roles/iap.httpsResourceAccessor

# 運営
gcloud run services add-iam-policy-binding champions-admin \
  --region=asia-northeast1 --project=${PROJECT_ID} \
  --member=user:you@example.com --role=roles/iap.httpsResourceAccessor
```

`IAP_AUDIENCE` は IAP を有効にして Deploy するまで値が分からないので、初回は空のままで Deploy する。
アプリケーションは `IAP_AUDIENCE` が空でも起動するが、リクエストはすべて 401 で拒否する。

一度ブラウザでアクセスすると、Cloud Logging に設定すべき値が出る。

```sh
gcloud logging read 'resource.type="cloud_run_revision" jsonPayload.msg="IAP_AUDIENCE is not set"' \
  --project=${PROJECT_ID} --limit=1 --format='value(jsonPayload.actualAudience)'
```

この値を `_IAP_AUDIENCE_SERVER` / `_IAP_AUDIENCE_ADMIN` に設定して Deploy し直すと認証が通るようになる。
外部 LB を挟む構成では `/projects/{PROJECT_NUMBER}/global/backendServices/{BACKEND_SERVICE_ID}` の形になる。

### 6. イベントの登録

`champions-admin` を開いてイベントコードを追加すると、`FOLDER_PARENT` の下の `champions` フォルダの中に
イベントコードと同名のフォルダができる。`champions` フォルダ自体も最初の登録時に自動で作られる。
付与する IAM Role、Enable にする API、Quota を設定し、`払い出しを受け付ける` にチェックを入れて保存すると、
参加者が `champions-server` でそのイベントコードを使えるようになる。

Quota の Service と Quota ID は次のコマンドで確認できる。

```sh
gcloud beta quotas info list --service=compute.googleapis.com --project=${PROJECT_ID}
```

## ローカル開発

ローカルには IAP がないので、`IAP_AUDIENCE` の代わりに `DEV_USER_EMAIL` を指定する。
このとき IAP の JWT 検証は行わず、すべてのリクエストがその email のユーザとしてログイン済みとして扱われる。
`IAP_AUDIENCE` と `DEV_USER_EMAIL` の両方が空だと起動時にエラーになるので、Cloud Run に間違って
疑似ユーザのまま Deploy されることはない (起動時に警告ログも出る)。

さらに、Cloud Tasks は localhost に届かないため、`LOCAL_TASKS=true` を付けると
払い出し処理を Cloud Tasks ではなくアプリケーション内の goroutine で実行する。
これで `POST /api/allocations` から `READY` になるまでの流れが、画面のポーリングも含めてローカルで一通り動く。

```sh
# 一般公開アプリケーション
GOOGLE_CLOUD_PROJECT=${PROJECT_ID} \
DEV_USER_EMAIL=you@example.com \
LOCAL_TASKS=true \
BILLING_ACCOUNT=billingAccounts/${BILLING_ACCOUNT_ID} \
go run ./cmd/server

# Admin
GOOGLE_CLOUD_PROJECT=${PROJECT_ID} \
DEV_USER_EMAIL=you@example.com \
LOCAL_TASKS=true \
FOLDER_PARENT=organizations/${ORG_ID} \
PORT=8081 \
go run ./cmd/admin
```

http://localhost:8080 と http://localhost:8081 を開けば、そのまま `you@example.com` としてログインした状態になる。

### 別のユーザとして動かす

`DEV_USER_EMAIL` を指定しているときだけ、`X-Dev-User-Email` ヘッダでログインするユーザを切り替えられる。
参加者を変えて払い出しを試したり、`ADMIN_EMAILS` の許可リストを確認したりするのに使う。

```sh
curl -s http://localhost:8080/api/me
# {"email":"you@example.com"}

curl -s -H 'X-Dev-User-Email: sato@example.com' http://localhost:8080/api/me
# {"email":"sato@example.com"}

# 別の参加者として払い出す
curl -s -X POST http://localhost:8080/api/allocations \
  -H 'Content-Type: application/json' \
  -H 'X-Dev-User-Email: sato@example.com' \
  -d '{"eventCode":"handson"}'
```

ブラウザから切り替えたい場合は、ヘッダを差し込める拡張を使うか、`DEV_USER_EMAIL` を変えて起動し直す。

### 認証だけをローカルで確認する

`DEV_USER_EMAIL` はローカルの動作確認用の抜け道なので、IAP の JWT 検証そのものを試したい場合は
`IAP_AUDIENCE` を設定し、Deploy 済みの環境で取得した JWT を `X-Goog-IAP-JWT-Assertion` ヘッダに載せて叩く。
audience が合っていないと 401 になり、Cloud Logging に JWT が持っている `actualAudience` が出る。

### 注意

`LOCAL_TASKS=true` でも、Project の作成や API の Enable は本物の Google Cloud に対して行われる。
実行する Google Account / ADC には [セットアップ](#3-service-account) の `champions-app` と同じ権限が必要で、
試した分だけ本物の Project ができるので、ハンズオン用とは別の検証用フォルダを `FOLDER_PARENT` に指定しておくとよい。

```sh
gcloud auth application-default login
```

Cloud Tasks 経由の worker の挙動だけを確かめたい場合は、`LOCAL_TASKS` を付けずに起動して worker を直接叩く。
`WORKER_BASE_URL` が `http://` のときは OIDC トークンの検証も行われないので、そのまま POST できる。

```sh
curl -X POST http://localhost:8080/tasks/provision \
  -H 'Content-Type: application/json' \
  -d '{"allocationID":"handson:you@example.com"}'
```

テストは `go test ./...` で実行できる。

## 注意点

- Project の作成には組織の Project 数の割当が必要になる。ハンズオンの規模に合わせて事前に確認しておく。
- Cloud Quotas の引き上げは申請であり、即座に反映されるとは限らない。`Reconciling` のまま承認待ちになることがある。
- `DELETE /api/events/{code}` は Firestore のイベントを消すだけで、払い出した Project とフォルダは残る。
  ハンズオン後の Project 削除は Admin の `全 Project を Shutdown` から行う。
- Shutdown は Project を削除依頼状態にするだけで、フォルダは残る。フォルダの削除は別途行う。
