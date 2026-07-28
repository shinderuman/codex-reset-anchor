# Codex Reset Anchor

Codex の週次利用枠（レートリミット）が回復したことを検知し、自動でアンカー用プロンプトを実行するツールです。

`codex app-server` の JSON-RPC を介して利用枠を監視し、回復を検知すると `codex exec` で軽量なプロンプトを1件実行します。これにより、回復直後の枠を確保（アンカー）します。

## 動作の概要

1. 指定間隔（既定: 5分）ごとに `codex app-server` を起動し、`initialize` ハンドシェイクを行います。
2. `account/rateLimits/read` で現在の週次利用枠を取得し、app-server を終了します。取得結果は状態ファイルに保存します。
3. 週次ウィンドウで利用率が閾値（1.0%）を下回り、かつリセット時刻が既知の境界から十分に進行（2分以上）した場合に「回復」と判定します。境界の進行が小さい場合は回復扱いにせず、既知の境界と高使用率の基準を保持して重複検知を防ぎます。
4. 回復を検知すると `codex exec` でアンカー用プロンプトを実行し、状態ファイルを更新します。

## 必要環境

- Go 1.25.5 以上
- `codex` CLI（`app-server` / `exec` サブコマンドが利用可能なこと）

## ビルド

```sh
go build -o codex-reset-anchor .
```

## 使い方

```sh
./codex-reset-anchor [オプション]
```

### オプション

| オプション    | 既定値                                                                | 説明                                   |
| ------------- | --------------------------------------------------------------------- | -------------------------------------- |
| `-codex`      | `codex`                                                               | codex コマンドのパス                   |
| `-state`      | `~/.local/var/codex-reset-anchor/state.json`                          | 状態ファイルのパス                     |
| `-interval`   | `5m`                                                                  | 利用枠の確認間隔（1分以上）            |
| `-prompt`     | `Reply only: OK`                                                      | アンカー実行に使うプロンプト           |
| `-model`      | （空）                                                                | アンカー実行に使うモデル。空なら Codex 既定値 |

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

## ライセンス

[MIT](LICENSE)
