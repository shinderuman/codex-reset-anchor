# Codex Reset Anchor

Codex の 5 時間利用枠と週次利用枠（レートリミット）が回復したことを検知し、自動でアンカー用プロンプトを実行するツールです。

`codex app-server` の JSON-RPC を介して利用枠を監視し、5 時間枠または週次枠のリセットを検知すると、未使用の新しい枠に対して `codex exec` で軽量なプロンプトを 1 件実行します。これにより、回復直後の枠を確保（アンカー）します。

## 動作の概要

1. 指定間隔（既定: 5 分）ごとに `codex app-server` を起動し、`initialize` ハンドシェイクを行います。
2. `account/rateLimits/read` で現在の利用枠を取得し、app-server を終了します。
3. `windowDurationMins` から 300 分の 5 時間枠と 10080 分の週次枠を識別します。`primary` / `secondary` の位置には依存しません。
4. 5 時間枠と週次枠を独立して監視します。前回取得した `resetsAt` を実時間が通過し、かつ次回 `resetsAt` が 2 分以上先へ進んだ場合に「回復」と判定します。リセット後の利用率には依存しないため、次回 poll までに Codex を使っていてもリセット自体は取り逃がしません。
5. `resetsAt` や `limitId` が一時的に欠落した poll では、同一 duration の window に対して最後に確認できた値を保持し、後続 poll でリセットを確定できるようにします。
6. 回復した枠の `usedPercent` が 0 より大きい場合は、すでに新しい枠が利用されているため追加のアンカーは行わず、リセット検知・利用済み・アンカースキップをログへ出力します。
7. 同じ poll で複数の枠が回復した場合、1 つでも `usedPercent == 0` の枠があれば `codex exec` を 1 件だけ実行します。すべての回復枠が利用済みなら実行しません。
8. anchor 成功または利用済みによるskip後は、その poll で観測した quota を次回判定の基準として保存します。新しい `resetsAt` を次回境界として保持するため、同じリセットを再処理しません。
9. 状態は state file に保存します。version 2 および 0.2.x 以前の単一 window state は起動時に現行形式へ自動移行します。

anchor 実行に失敗した場合は state を進めず、次回 poll で再試行できる状態を保持します。

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
| `-model` | （空） | アンカー実行に使うモデル。空なら Codex 既定値 |
| `-anchor-timeout` | `2m` | アンカー実行のタイムアウト |

### 実行例

```sh
# 既定設定で起動
./codex-reset-anchor

# 確認間隔を10分に変更して起動
./codex-reset-anchor -interval 10m

# アンカー用モデルを明示指定
./codex-reset-anchor -model gpt-5
```

終了は `Ctrl+C`（SIGINT）または SIGTERM です。

## プロジェクト構成

```text
cmd/codex-reset-anchor/main.go  CLI entry point
internal/app/                   設定・監視ループ・ユースケース
internal/codex/                 codex app-server / exec 連携
internal/quota/                 quota と回復判定
internal/state/                 state file の読み書き・migration
```

`cmd/codex-reset-anchor/main.go` は起動処理だけを持ち、実装は `internal/` 配下へ分離しています。

## state file

現行 state schema は version 3 です。

```json
{
  "version": 3,
  "fiveHour": {
    "limitId": "codex",
    "windowName": "primary",
    "usedPercent": 50,
    "windowDurationMins": 300,
    "resetsAt": 1787775687,
    "checkedAt": 0
  },
  "weekly": {
    "limitId": "codex",
    "windowName": "secondary",
    "usedPercent": 71,
    "windowDurationMins": 10080,
    "resetsAt": 1788271937,
    "checkedAt": 0
  }
}
```

`windowName` は観測情報として保存しますが、window の同一性判定には使いません。そのため Codex 側で 5 時間枠と週次枠の `primary` / `secondary` 配置が変わっても、同じ duration の枠として追跡を継続します。`limitId` が片方の poll で欠落した場合も、同じ duration であれば既知の ID を保持します。

## ライセンス

[MIT](LICENSE)
