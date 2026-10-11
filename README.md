# Codex Reset Anchor

Codex の 5 時間利用枠と週次利用枠（レートリミット）の更新を検知し、自動でアンカー用プロンプトを実行するツールです。

`codex app-server` の JSON-RPC を介して利用枠を監視し、5 時間枠または週次枠のリセットを検知すると、未使用の新しい枠に対して `codex exec` で軽量なプロンプトを 1 件実行します。これにより、回復直後の枠を確保（アンカー）します。

## 動作の概要

1. 指定間隔（既定: 5 分）で `codex app-server` の `account/rateLimits/read` から利用枠を取得します。300 分の枠を 5h、10080 分の枠を weekly として識別します。
2. 初回は比較の基準を保存します。以降は各枠の `resetsAt` を分単位で比較し、分が変われば更新として検知します。前後どちらへの変更も対象です。
3. 更新された枠に `usedPercent == 0` の枠があればアンカーを 1 件実行します。すべて使用済みならスキップします。
4. 成功・スキップ時は取得値を次回の基準として保存します。アンカー失敗時は基準を進めず、次回に再試行します。
5. 更新された枠、前回の 5h / weekly 残量、アンカー結果を 1 行で出力します。

5 時間枠・週次枠のどちらがリセットしても、前回確認時の残り利用率とアンカー結果を1行にまとめて出力します。残り利用率は `100 - usedPercent`（0〜100%）で、小数点なしの整数表示です。

```text
2026/10/08 13:39:35 reset=5h prev=2%/45% anchor=ok
```

`reset` はリセットした枠（同時なら `5h,weekly`）、`prev` は常に 5h / weekly の順です。`anchor` は成功時 `ok`、利用済みによるスキップ時 `skip`、実行失敗時 `err` です。失敗の詳細は同じ行の `err="..."` に出力します。アンカー成功後の状態保存に失敗した場合は `anchor=ok` に保存エラーを併記します。ログ時刻は処理結果が確定した時刻です。

定期確認のため、前回確認時の値は保存済みの最終確認値であり、リセット時刻ちょうどの残量ではありません。取得できていない枠は `-` と表示します。

## Anchor のトークン節約

Anchor 用の `codex exec` は通常の Codex 作業コンテキストを持ち込まないように実行します。

- `--ephemeral` でセッションを永続化しません。
- `--ignore-user-config` で通常作業用の `$CODEX_HOME/config.toml` を読み込まず、認証情報だけを通常の `CODEX_HOME` から利用します。
- `--ignore-rules` で user/project execpolicy rules を読み込みません。
- 既定モデルは `gpt-5.6-luna`、reasoning effort と verbosity は `low` に固定します。`-model` でモデルだけ変更できます。
- Codex の標準 base instructions は短い Anchor 専用 instructions で置き換えます。
- permissions / apps / collaboration mode / environment context / skills の model-visible instructions を無効化します。
- bundled skills と web search を無効化します。
- `project_doc_max_bytes=0` として project の `AGENTS.md` を Anchor prompt に含めません。

Codex CLI が標準で持つ core tool definitions までは公開 CLI から完全には無効化できないため、それらは残ります。また `$CODEX_HOME/AGENTS.md` / `AGENTS.override.md` は認証用 `CODEX_HOME` と同じ場所から読み込まれるため、このツールでは分離しません。

## 必要環境

- Go 1.25.5 以上
- `codex` CLI（`app-server` / `exec` サブコマンドが利用可能なこと）

## ビルド

```sh
go build -o codex-reset-anchor ./cmd/codex-reset-anchor
```

## テスト

```sh
go test ./...
go vet ./...
```

## 使い方

```sh
./codex-reset-anchor [オプション]
```

### オプション

| オプション | 既定値 | 説明 |
| --- | --- | --- |
| `-codex` | `codex` | codex コマンドのパス |
| `-state` | `~/.local/var/codex-reset-anchor/state.json` | 状態ファイルのパス |
| `-interval` | `5m` | 利用枠の確認間隔（1 分以上） |
| `-prompt` | `Reply only: OK` | アンカー実行に使うプロンプト |
| `-model` | `gpt-5.6-luna`（実効既定値） | アンカー実行に使うモデル |
| `-anchor-timeout` | `2m` | アンカー実行のタイムアウト |

### 実行例

```sh
# 既定設定で起動
./codex-reset-anchor

# 確認間隔を10分に変更して起動
./codex-reset-anchor -interval 10m

# アンカー用モデルを明示指定
./codex-reset-anchor -model gpt-5.6-terra
```

終了は `Ctrl+C`（SIGINT）または SIGTERM です。

## プロジェクト構成

```text
cmd/codex-reset-anchor/main.go  CLI entry point
internal/app/                   起動・CLI 設定・依存の組み立て
internal/monitor/               監視ループ・アンカー実行・ログ出力
internal/codex/                 codex app-server / exec 連携
internal/quota/                 quota・更新判定
internal/state/                 state file の読み書き
```

`app` が設定と起動、`monitor` が監視・アンカー実行・ログ出力、`quota` が取得値の比較、`state` が保存、`codex` が CLI との通信を担当します。

## state file

5h と weekly の前回取得値を保存します。

```json
{
  "fiveHour": {
    "limitId": "codex",
    "windowName": "primary",
    "usedPercent": 50,
    "windowDurationMins": 300,
    "resetsAt": 1787775687
  },
  "weekly": {
    "limitId": "codex",
    "windowName": "secondary",
    "usedPercent": 71,
    "windowDurationMins": 10080,
    "resetsAt": 1788271937
  }
}
```

`windowName` は取得元の位置です。利用枠は `limitId` と `windowDurationMins` で識別します。

## ライセンス

[MIT](LICENSE)
