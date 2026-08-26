# Codex Reset Anchor

Codex の 5 時間利用枠と週次利用枠（レートリミット）が回復したことを検知し、自動でアンカー用プロンプトを実行するツールです。

`codex app-server` の JSON-RPC を介して利用枠を監視し、5 時間枠または週次枠のリセットを検知すると `codex exec` で軽量なプロンプトを 1 件実行します。これにより、回復直後の枠を確保（アンカー）します。

## 動作の概要

1. 指定間隔（既定: 5 分）ごとに `codex app-server` を起動し、`initialize` ハンドシェイクを行います。
2. `account/rateLimits/read` で現在の利用枠を取得し、app-server を終了します。
3. `windowDurationMins` から 300 分の 5 時間枠と 10080 分の週次枠を識別します。`primary` / `secondary` の位置には依存しません。
4. 5 時間枠と週次枠を独立した reset detector で監視します。利用率が閾値（1.0%）以下へ下がり、かつリセット時刻が既知の境界から 2 分以上進行した場合に「回復」と判定します。
5. どちらか一方または両方の回復を検知すると `codex exec` でアンカー用プロンプトを 1 件だけ実行します。同じ poll で両方が回復しても anchor は 1 回です。
6. anchor 成功後は、存在する 5 時間枠・週次枠の両 detector を rearm します。表示上の利用率が 0% のままでも、次回リセットを検知できる状態を維持します。
7. 状態は state file に保存します。0.2.x 以前の単一 weekly window state は起動時に新しい multi-window state へ自動移行します。

anchor 実行に失敗した場合は、検知状態を進めず、次回 poll で再試行できる状態を保持します。

## 必要環境

- Go 1.25.5 以上
- `codex` CLI（`app-server` / `exec` サブコマンドが利用可能なこと）

## ビルド

```sh
go build -o codex-reset-anchor .
```

## テスト

```sh
go test ./...
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

## state file

現行 state schema は version 2 です。

```json
{
  "version": 2,
  "fiveHour": {
    "limitId": "codex",
    "windowName": "primary",
    "usedPercent": 50,
    "windowDurationMins": 300,
    "resetsAt": 1787775687,
    "checkedAt": 0,
    "resetDetector": {
      "wasAboveThreshold": true,
      "resetBoundary": 1787775687
    }
  },
  "weekly": {
    "limitId": "codex",
    "windowName": "secondary",
    "usedPercent": 71,
    "windowDurationMins": 10080,
    "resetsAt": 1788271937,
    "checkedAt": 0,
    "resetDetector": {
      "wasAboveThreshold": true,
      "resetBoundary": 1788271937
    }
  }
}
```

`windowName` は観測情報として保存しますが、reset detector の同一性判定には使いません。そのため Codex 側で 5 時間枠と週次枠の `primary` / `secondary` 配置が変わっても、同じ duration の枠として追跡を継続します。

## ライセンス

[MIT](LICENSE)
