# 群像战棋

## 本地配置 API key

后端默认读取 `backend/config.local.json`。这个文件只用于本地运行，已在 `.gitignore` 中排除，不应该提交真实密钥。

可以参考 `backend/config.example.json`，把 `backend/config.local.json` 里的 `openai.api_key` 填成自己的 key：

```json
{
  "openai": {
    "base_url": "https://api.openai.com/v1",
    "wire_api": "chat_completions",
    "api_key": "你的 API key",
    "model": "gpt-4o-mini",
    "timeout_seconds": "180"
  }
}
```

也可以通过环境变量指定其他配置文件：

```bash
export QUNXIANG_CONFIG_FILE=/absolute/path/to/config.local.json
```

环境变量优先级最高；如果同时配置了 `QUNXIANG_OPENAI_API_KEY` 或 `OPENAI_API_KEY`，会覆盖 JSON 里的值。

## 启动后端

```bash
cd backend
go run ./cmd/server
```

默认监听 `http://127.0.0.1:8080`。

## 启动前端

另开一个终端：

```bash
cd frontend
npm install
npm run dev -- --host 127.0.0.1 --port 5173
```

打开 `http://127.0.0.1:5173` 进入游戏。

## 提交到 GitHub 前

只提交 `backend/config.example.json` 作为模板，不要提交带真实 key 的 `backend/config.local.json`。
