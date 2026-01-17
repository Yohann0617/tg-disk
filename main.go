package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/joho/godotenv"
)

//go:embed static/*
//go:embed .env
var embeddedFiles embed.FS

// 静态文件系统包装，自动给路径加 static/ 前缀
type staticFS struct {
	fs http.FileSystem
}

func (s staticFS) Open(name string) (http.File, error) {
	if strings.HasPrefix(name, "/static") {
		name = name[1:]
	}
	return s.fs.Open(name)
}

var (
	bot                *tgbotapi.BotAPI
	chatID             int64
	accessPwd          string
	downloadThreads    = 8  // Download concurrent threads (can be higher)
	frontendChunkSize  = 20 // Frontend chunk size in MB
	frontendConcurrent = 8  // Frontend chunk upload concurrency
	frontendFilesLimit = 5  // Frontend file upload concurrency
)

func main() {
	// 定义命令行参数（默认值为空）
	portFlag := flag.String("port", "", "服务端口")
	botTokenFlag := flag.String("bot_token", "", "Telegram Bot Token")
	accessPwdFlag := flag.String("access_pwd", "", "访问密码")
	proxyFlag := flag.String("proxy", "", "HTTP 代理地址")
	chatIDFlag := flag.String("chat_id", "", "Telegram Chat ID")
	baseURLFlag := flag.String("base_url", "", "服务的基础 URL，例如 https://yourdomain.com")
	flag.Parse()

	envLoaded := false

	// 尝试加载 .env 文件
	if _, err := os.Stat(".env"); err == nil {
		if err := godotenv.Load(".env"); err != nil {
			log.Fatal("加载外部 .env 文件失败:", err)
		}
		log.Println("使用外部 .env 配置")
		envLoaded = true
	} else {
		// 使用嵌入 .env
		envBytes, err := embeddedFiles.ReadFile(".env")
		if err != nil {
			log.Fatal("读取嵌入 .env 文件失败:", err)
		}
		envMap, err := godotenv.Parse(strings.NewReader(string(envBytes)))
		if err != nil {
			log.Fatal("解析嵌入 .env 失败:", err)
		}
		for k, v := range envMap {
			os.Setenv(k, v)
		}
		log.Println("使用嵌入的 .env 配置")
	}

	// 如果命令行指定了参数，就覆盖环境变量
	overrideEnv := func(key, value string) {
		if value != "" {
			os.Setenv(key, value)
		}
	}
	overrideEnv("PORT", *portFlag)
	overrideEnv("BOT_TOKEN", *botTokenFlag)
	overrideEnv("ACCESS_PWD", *accessPwdFlag)
	overrideEnv("PROXY", *proxyFlag)
	overrideEnv("CHAT_ID", *chatIDFlag)
	overrideEnv("BASE_URL", *baseURLFlag)

	// 读取最终环境变量
	port := os.Getenv("PORT")
	botToken := os.Getenv("BOT_TOKEN")
	accessPwd = os.Getenv("ACCESS_PWD")
	proxyStr := os.Getenv("PROXY")
	chatIDStr := os.Getenv("CHAT_ID")
	baseURL := os.Getenv("BASE_URL")

	// Read thread configuration from environment
	if downloadThreadsStr := os.Getenv("DOWNLOAD_THREADS"); downloadThreadsStr != "" {
		if val, err := strconv.Atoi(downloadThreadsStr); err == nil && val > 0 {
			downloadThreads = val
		}
	}
	if chunkSizeStr := os.Getenv("CHUNK_SIZE_MB"); chunkSizeStr != "" {
		if val, err := strconv.Atoi(chunkSizeStr); err == nil && val > 0 && val <= 50 {
			frontendChunkSize = val
		}
	}
	if concurrentStr := os.Getenv("CHUNK_CONCURRENT"); concurrentStr != "" {
		if val, err := strconv.Atoi(concurrentStr); err == nil && val > 0 {
			frontendConcurrent = val
		}
	}
	if filesLimitStr := os.Getenv("FILES_CONCURRENT"); filesLimitStr != "" {
		if val, err := strconv.Atoi(filesLimitStr); err == nil && val > 0 {
			frontendFilesLimit = val
		}
	}

	log.Printf("配置信息 - 下载线程: %d, 分片大小: %dMB, 分片并发: %d, 文件并发: %d",
		downloadThreads, frontendChunkSize, frontendConcurrent, frontendFilesLimit)

	// 检查必填
	if port == "" && !envLoaded {
		log.Fatal("未找到 .env 文件，必须通过 -port 指定服务端口")
	}
	if botToken == "" || accessPwd == "" || chatIDStr == "" {
		log.Fatal("缺少必要配置，请通过 .env 或命令行设置 bot_token、access_pwd、chat_id")
	}

	var err error
	chatID, err = strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		log.Fatal("CHAT_ID 格式错误，应为数字:", err)
	}

	if proxyStr != "" {
		proxyURL, err := url.Parse(proxyStr)
		if err != nil {
			log.Fatal("代理地址格式错误:", err)
		}

		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			},
		}
		bot, err = tgbotapi.NewBotAPIWithClient(botToken, tgbotapi.APIEndpoint, client)
		if err != nil {
			log.Fatal("初始化 Bot 失败:", err)
		}
		http.DefaultTransport = &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
	} else {
		bot, err = tgbotapi.NewBotAPI(botToken)
		if err != nil {
			log.Fatal("初始化 Bot 失败:", err)
		}
	}

	go func() {
		_, _ = bot.Send(tgbotapi.NewMessage(chatID, "🤖tg-disk服务启动成功🎉🎉\n\n"+
			"指定文件回复get获取URL链接\n源码地址：https://github.com/Yohann0617/tg-disk"))

		u := tgbotapi.NewUpdate(0)
		u.Timeout = 60
		updates := bot.GetUpdatesChan(u)

		for update := range updates {
			if update.Message == nil || update.Message.ReplyToMessage == nil {
				continue
			}
			if update.Message.From.ID != chatID {
				_, _ = bot.Send(tgbotapi.NewMessage(update.Message.From.ID, "您无权限使用此机器人"))
				continue
			}

			// 只处理私聊
			msgText := strings.TrimSpace(update.Message.Text)
			if update.Message.Chat.IsPrivate() && (msgText == "get" || msgText == "/get") {
				if baseURL == "" {
					msg := tgbotapi.NewMessage(update.Message.From.ID, "未配置 BASE_URL 参数，无法获取完整URL链接")
					_, _ = bot.Send(msg)
					continue
				}

				var msg *tgbotapi.Message
				if update.Message != nil {
					msg = update.Message
				}

				var fileID, fileName string
				replyToMessage := msg.ReplyToMessage

				switch {
				case replyToMessage.Document != nil && replyToMessage.Document.FileID != "":
					fileID = replyToMessage.Document.FileID
					fileName = replyToMessage.Document.FileName
				case replyToMessage.Video != nil && replyToMessage.Video.FileID != "":
					fileID = replyToMessage.Video.FileID
					fileName = replyToMessage.Video.FileName
				case replyToMessage.Audio != nil && replyToMessage.Audio.FileID != "":
					fileID = replyToMessage.Audio.FileID
					fileName = replyToMessage.Audio.FileName
				case replyToMessage.Animation != nil && replyToMessage.Animation.FileID != "":
					fileID = replyToMessage.Animation.FileID
					fileName = replyToMessage.Animation.FileName
				case replyToMessage.Sticker != nil && replyToMessage.Sticker.FileID != "":
					fileID = replyToMessage.Sticker.FileID
					fileName = replyToMessage.Sticker.Emoji
				}

				var downloadURL string
				if fileName == "fileAll.txt" {
					downloadURL = fmt.Sprintf("%s/d?file_id=%s", strings.TrimRight(baseURL, "/"), fileID)
				} else {
					downloadURL = fmt.Sprintf("%s/d?file_id=%s&filename=%s",
						strings.TrimRight(baseURL, "/"), fileID, url.QueryEscape(fileName))
				}

				var msgRsp tgbotapi.MessageConfig
				if fileID != "" {
					msgRsp = tgbotapi.NewMessage(update.Message.From.ID, "文件 ["+fileName+"] 下载链接：\n"+downloadURL)
				} else {
					msgRsp = tgbotapi.NewMessage(update.Message.From.ID, "无法获取文件ID")
				}
				_, err := bot.Send(msgRsp)
				if err != nil {
					log.Println(err)
				}
			}
		}
	}()

	httpFS, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/", http.FileServer(staticFS{http.FS(httpFS)}))
	http.HandleFunc("/verify", handleVerify)
	http.HandleFunc("/config", handleConfig)
	http.HandleFunc("/upload", handleUpload)
	http.HandleFunc("/upload_chunk", handleUploadChunk)
	http.HandleFunc("/merge_chunks", handleMergeChunks)
	http.HandleFunc("/d", handleDownload)

	if port == "" {
		port = "8080" // fallback
	}
	log.Printf("🎉🎉 The service is started successfully -> http://127.0.0.1:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

type UploadResult struct {
	Filename    string `json:"filename"`
	FileID      string `json:"file_id"`
	DownloadURL string `json:"download_url"`
}

// handleUpload handles small file upload (<=20MB)
func handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if r.FormValue("pwd") != accessPwd {
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "读取文件失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	tmpDir, err := os.MkdirTemp("", "upload_")
	if err != nil {
		http.Error(w, "创建临时目录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	origFilename := header.Filename
	tmpPath := filepath.Join(tmpDir, origFilename)
	tmp, err := os.Create(tmpPath)
	if err != nil {
		http.Error(w, "创建临时文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tmp.Close()

	_, err = io.Copy(tmp, file)
	if err != nil {
		http.Error(w, "写入临时文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	var fileId string
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(tmpPath))
	doc.Caption = origFilename
	msg, err := bot.Send(doc)
	if err != nil {
		log.Println("上传到 Telegram 失败: "+err.Error(), err)
		http.Error(w, "上传到 Telegram 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if msg.Document != nil {
		fileId = msg.Document.FileID
	} else if msg.Video != nil {
		fileId = msg.Video.FileID
	} else if msg.Audio != nil {
		fileId = msg.Audio.FileID
	}

	downloadURL := fmt.Sprintf("%s://%s/d?file_id=%s&filename=%s",
		getScheme(r), r.Host, fileId, origFilename)

	result := UploadResult{
		Filename:    origFilename,
		FileID:      fileId,
		DownloadURL: downloadURL,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleUploadChunk handles single chunk upload from frontend
func handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if r.FormValue("pwd") != accessPwd {
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}

	chunk, _, err := r.FormFile("chunk")
	if err != nil {
		http.Error(w, "读取分片失败: "+err.Error(), http.StatusBadRequest)
		return
	}
	defer chunk.Close()

	chunkIndex := r.FormValue("chunk_index")
	totalChunks := r.FormValue("total_chunks")
	filename := r.FormValue("filename")

	tmpDir, err := os.MkdirTemp("", "chunk_")
	if err != nil {
		http.Error(w, "创建临时目录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	chunkPath := filepath.Join(tmpDir, "blob")
	tmp, err := os.Create(chunkPath)
	if err != nil {
		http.Error(w, "创建临时文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer tmp.Close()

	_, err = io.Copy(tmp, chunk)
	if err != nil {
		http.Error(w, "写入临时文件失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmp.Close()

	// Build caption with chunk info
	caption := fmt.Sprintf("blob [%s/%s] - %s", chunkIndex, totalChunks, filename)

	// Upload chunk to Telegram
	doc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(chunkPath))
	doc.Caption = caption
	msg, err := bot.Send(doc)
	if err != nil || msg.Document == nil {
		http.Error(w, "上传分片到 Telegram 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	type ChunkResult struct {
		FileID string `json:"file_id"`
	}

	result := ChunkResult{
		FileID: msg.Document.FileID,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

// handleMergeChunks creates fileAll.txt and uploads it to Telegram
func handleMergeChunks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if r.FormValue("pwd") != accessPwd {
		http.Error(w, "密码错误", http.StatusUnauthorized)
		return
	}

	filename := r.FormValue("filename")
	chunkIDsJSON := r.FormValue("chunk_ids")

	if filename == "" || chunkIDsJSON == "" {
		http.Error(w, "缺少 filename 或 chunk_ids 参数", http.StatusBadRequest)
		return
	}

	var chunkIDs []string
	if err := json.Unmarshal([]byte(chunkIDsJSON), &chunkIDs); err != nil {
		http.Error(w, "chunk_ids 格式错误: "+err.Error(), http.StatusBadRequest)
		return
	}

	if len(chunkIDs) == 0 {
		http.Error(w, "chunk_ids 不能为空", http.StatusBadRequest)
		return
	}

	// Build fileAll.txt content
	builder := strings.Builder{}
	builder.WriteString(filename + "\n")
	for _, fid := range chunkIDs {
		builder.WriteString(fid + "\n")
	}

	tmpDir, err := os.MkdirTemp("", "merge_")
	if err != nil {
		http.Error(w, "创建临时目录失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(tmpDir)

	metaPath := filepath.Join(tmpDir, "fileAll.txt")
	if err := os.WriteFile(metaPath, []byte(builder.String()), 0644); err != nil {
		http.Error(w, "写入 fileAll.txt 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Upload fileAll.txt to Telegram
	metaDoc := tgbotapi.NewDocument(chatID, tgbotapi.FilePath(metaPath))
	metaDoc.Caption = filename
	msg, err := bot.Send(metaDoc)
	if err != nil || msg.Document == nil {
		http.Error(w, "上传 fileAll.txt 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	fileID := msg.Document.FileID
	downloadURL := fmt.Sprintf("%s://%s/d?file_id=%s", getScheme(r), r.Host, fileID)

	result := UploadResult{
		Filename:    filename,
		FileID:      fileID,
		DownloadURL: downloadURL,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	fileID := r.URL.Query().Get("file_id")
	filename := r.URL.Query().Get("filename")

	if fileID == "" {
		http.Error(w, "缺少 file_id 参数", http.StatusBadRequest)
		return
	}

	// filename 参数存在，表示是小文件，直接下载
	if filename != "" {
		tgFile, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
		if err != nil {
			// Check if error is due to file being too large
			errMsg := err.Error()
			if strings.Contains(errMsg, "file is too big") || strings.Contains(errMsg, "Request Entity Too Large") {
				// Return HTML page with error message and instructions
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>文件下载失败</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        .error { background: #fff3cd; border: 1px solid #ffc107; border-radius: 8px; padding: 20px; }
        .error h2 { color: #856404; margin-top: 0; }
        .solution { background: #d1ecf1; border: 1px solid #17a2b8; border-radius: 8px; padding: 20px; margin-top: 20px; }
        .solution h3 { color: #0c5460; margin-top: 0; }
        code { background: #f4f4f4; padding: 2px 6px; border-radius: 3px; }
        ol { line-height: 1.8; }
        .telegram-link { display: inline-block; background: #0088cc; color: white; padding: 10px 20px; text-decoration: none; border-radius: 5px; margin-top: 10px; }
        .telegram-link:hover { background: #006699; }
    </style>
</head>
<body>
    <div class="error">
        <h2>⚠️ 文件大小超过限制</h2>
        <p>此文件大小超过 Telegram Bot API 的 20MB 下载限制，无法通过此链接下载。</p>
        <p><strong>文件名：</strong> %s</p>
    </div>
    
    <div class="solution">
        <h3>💡 解决方案</h3>
        <p><strong>方法一：使用网页上传功能（推荐）</strong></p>
        <ol>
            <li>访问 <code>%s</code></li>
            <li>通过网页上传此文件</li>
            <li>系统会自动分片处理，支持任意大小文件</li>
            <li>上传完成后获取新的下载链接</li>
        </ol>
        
        <p><strong>方法二：直接在 Telegram 中下载</strong></p>
        <p>在 Telegram 客户端中打开此文件即可直接下载（不受 20MB 限制）</p>
        <a href="https://t.me/c/%d/%s" class="telegram-link" target="_blank">📱 在 Telegram 中打开</a>
    </div>
</body>
</html>
`, filename, getScheme(r)+"://"+r.Host, chatID, fileID)
				return
			}
			http.Error(w, "获取文件失败: "+err.Error(), http.StatusInternalServerError)
			return
		}

		// Additional check: Bot API has 20MB download limit
		if tgFile.FileSize > 20*1024*1024 {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			fileSize := float64(tgFile.FileSize) / (1024 * 1024)
			fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>文件下载失败</title>
    <style>
        body { font-family: Arial, sans-serif; max-width: 800px; margin: 50px auto; padding: 20px; }
        .error { background: #fff3cd; border: 1px solid #ffc107; border-radius: 8px; padding: 20px; }
        .error h2 { color: #856404; margin-top: 0; }
        .solution { background: #d1ecf1; border: 1px solid #17a2b8; border-radius: 8px; padding: 20px; margin-top: 20px; }
        .solution h3 { color: #0c5460; margin-top: 0; }
        code { background: #f4f4f4; padding: 2px 6px; border-radius: 3px; }
        ol { line-height: 1.8; }
    </style>
</head>
<body>
    <div class="error">
        <h2>⚠️ 文件大小超过限制</h2>
        <p>此文件大小为 <strong>%.2f MB</strong>，超过 Telegram Bot API 的 20MB 下载限制。</p>
        <p><strong>文件名：</strong> %s</p>
    </div>
    
    <div class="solution">
        <h3>💡 解决方案</h3>
        <p><strong>请使用网页上传功能（推荐）</strong></p>
        <ol>
            <li>访问 <code>%s</code></li>
            <li>通过网页重新上传此文件</li>
            <li>系统会自动分片处理，支持任意大小文件</li>
            <li>上传完成后获取新的下载链接</li>
        </ol>
        <p style="color: #666; margin-top: 20px;">💡 提示：通过网页上传的大文件会自动分片，下载时无大小限制。</p>
    </div>
</body>
</html>
`, fileSize, filename, getScheme(r)+"://"+r.Host)
			return
		}

		url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", bot.Token, tgFile.FilePath)
		resp, err := http.Get(url)
		if err != nil {
			http.Error(w, "下载失败: "+err.Error(), http.StatusInternalServerError)
			return
		}
		defer resp.Body.Close()

		ext := filepath.Ext(filename)
		contentType := mime.TypeByExtension(ext)

		switch contentType {
		case "":
			if strings.Contains(strings.ToLower(ext), ".mp3") {
				contentType = "audio/mpeg"
			} else if strings.Contains(strings.ToLower(ext), ".flac") {
				contentType = "audio/x-flac"
			} else if strings.Contains(strings.ToLower(ext), ".mp4") {
				contentType = "video/mp4"
			} else {
				contentType = "application/octet-stream"
			}
		case "image/gif":
			contentType = "video/mp4"
		default:

		}

		w.Header().Set("Content-Type", contentType)
		// 仅在不能预览时强制下载
		if !isPreviewable(contentType) {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", filename))
		}
		w.Header().Set("Accept-Ranges", "bytes")
		io.Copy(w, resp.Body)
		return
	}

	// 否则为 fileAll.txt 模式（大文件组合下载）
	tgFile, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		http.Error(w, "获取 fileAll.txt 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	url := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", bot.Token, tgFile.FilePath)
	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, "下载 fileAll.txt 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("下载 fileAll.txt 返回状态异常: %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	linesBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "读取 fileAll.txt 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}

	linesStr := strings.Split(strings.TrimSpace(string(linesBytes)), "\n")

	// 去掉空行
	var cleanLines []string
	for _, line := range linesStr {
		line = strings.TrimSpace(line)
		if line != "" {
			cleanLines = append(cleanLines, line)
		}
	}

	if len(cleanLines) < 2 {
		http.Error(w, "fileAll.txt 格式错误，至少应有文件名和一个分块ID", http.StatusBadRequest)
		return
	}

	origFilename := cleanLines[0]
	blobFileIDs := cleanLines[1:]

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", origFilename))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "服务器不支持 Flush", http.StatusInternalServerError)
		return
	}

	log.Printf("开始流式下载合并大文件，文件名: %s，共 %d 个分块", origFilename, len(blobFileIDs))

	// Concurrent download with streaming output
	// Download multiple chunks concurrently, but write them in order
	type chunkResult struct {
		index int
		data  []byte
		err   error
	}

	// Channel to receive downloaded chunks
	resultChan := make(chan chunkResult, len(blobFileIDs))

	// Goroutine pool to download chunks concurrently
	var wg sync.WaitGroup
	sem := make(chan struct{}, downloadThreads)

	for i, fid := range blobFileIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func(index int, fileID string) {
			defer wg.Done()
			defer func() { <-sem }()

			tgBlob, err := bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
			if err != nil {
				resultChan <- chunkResult{index: index, err: fmt.Errorf("获取分块 %d 失败: %v", index, err)}
				return
			}

			blobURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", bot.Token, tgBlob.FilePath)
			resp, err := http.Get(blobURL)
			if err != nil {
				resultChan <- chunkResult{index: index, err: fmt.Errorf("下载分块 %d 失败: %v", index, err)}
				return
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				resultChan <- chunkResult{index: index, err: fmt.Errorf("下载分块 %d 状态码异常: %d", index, resp.StatusCode)}
				return
			}

			data, err := io.ReadAll(resp.Body)
			if err != nil {
				resultChan <- chunkResult{index: index, err: fmt.Errorf("读取分块 %d 失败: %v", index, err)}
				return
			}

			resultChan <- chunkResult{index: index, data: data}
			log.Printf("已下载分块 %d/%d，大小: %d 字节", index+1, len(blobFileIDs), len(data))
		}(i, fid)
	}

	// Close channel when all downloads complete
	go func() {
		wg.Wait()
		close(resultChan)
	}()

	// Collect results and maintain order
	chunks := make([][]byte, len(blobFileIDs))
	received := make([]bool, len(blobFileIDs))
	receivedCount := 0
	nextToWrite := 0

	for result := range resultChan {
		if result.err != nil {
			log.Printf("下载错误: %v", result.err)
			http.Error(w, result.err.Error(), http.StatusInternalServerError)
			return
		}

		chunks[result.index] = result.data
		received[result.index] = true
		receivedCount++

		// Write all consecutive chunks that are ready
		for nextToWrite < len(blobFileIDs) && received[nextToWrite] {
			log.Printf("写入分块 %d/%d，大小: %d 字节", nextToWrite+1, len(blobFileIDs), len(chunks[nextToWrite]))
			_, err := w.Write(chunks[nextToWrite])
			if err != nil {
				log.Printf("写入响应失败（分块 %d）: %v", nextToWrite, err)
				return
			}
			flusher.Flush()

			// Free memory immediately after writing
			chunks[nextToWrite] = nil
			nextToWrite++
		}
	}

	log.Printf("流式下载完成: %s，共 %d 个分块", origFilename, len(blobFileIDs))
}

func handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(10 << 20); err != nil {
		http.Error(w, "解析表单失败", http.StatusBadRequest)
		return
	}
	if r.FormValue("pwd") == accessPwd {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	} else {
		http.Error(w, "密码错误", http.StatusUnauthorized)
	}
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	type ConfigResponse struct {
		ChunkSizeMB     int `json:"chunk_size_mb"`
		ChunkConcurrent int `json:"chunk_concurrent"`
		FilesConcurrent int `json:"files_concurrent"`
		DownloadThreads int `json:"download_threads"`
	}

	config := ConfigResponse{
		ChunkSizeMB:     frontendChunkSize,
		ChunkConcurrent: frontendConcurrent,
		FilesConcurrent: frontendFilesLimit,
		DownloadThreads: downloadThreads,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(config)
}

func getScheme(r *http.Request) string {
	// 优先使用反向代理头部判断协议
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		return proto
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func isPreviewable(contentType string) bool {
	return strings.HasPrefix(contentType, "image/") ||
		strings.HasPrefix(contentType, "video/") ||
		strings.HasPrefix(contentType, "audio/") ||
		contentType == "application/pdf"
}
