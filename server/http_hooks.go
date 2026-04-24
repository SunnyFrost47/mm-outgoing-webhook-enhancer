package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
)

// ServeHTTP обрабатывает HTTP-запросы
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	p.router.ServeHTTP(w, r)
}

// initializeAPI инициализирует API для обработки HTTP-запросов
func (p *Plugin) initializeAPI() {
	router := mux.NewRouter()

	router.HandleFunc("/status", p.handleStatus).Methods("GET")
	router.HandleFunc("/messages", p.handleMessages).Methods("GET")

	p.router = router
}

func (p *Plugin) handleStatus(w http.ResponseWriter, r *http.Request) {
	var response = struct {
		Enabled bool `json:"enabled"`
	}{
		Enabled: true,
	}

	responseJSON, _ := json.Marshal(response)

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(responseJSON); err != nil {
		p.API.LogError("Failed to write status", "err", err.Error())
	}
}

// handleMessages возвращает сообщения для текущего пользователя из всех каналов,
// начиная с временной метки since, с ограничением limit (по умолчанию 100).
func (p *Plugin) handleMessages(w http.ResponseWriter, r *http.Request) {
	// 1. Проверка Bearer токена
	token := r.Header.Get("Authorization")
	if len(token) < 7 || token[:7] != "Bearer " {
		http.Error(w, "Unauthorized: missing or malformed Bearer token", http.StatusUnauthorized)
		return
	}
	sessionToken := token[7:]

	session, err := p.API.GetSession(sessionToken)
	if err != nil || session == nil {
		p.API.LogError("Invalid session", "error", err.Error())
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	userID := session.UserId

	// 2. Парсинг параметров запроса
	query := r.URL.Query()
	sinceStr := query.Get("since") // обязательный, миллисекунды UNIX
	if sinceStr == "" {
		http.Error(w, "Missing 'since' parameter (Unix milliseconds)", http.StatusBadRequest)
		return
	}
	since, parsErr := strconv.ParseInt(sinceStr, 10, 64)
	if parsErr != nil {
		http.Error(w, "Invalid 'since' parameter", http.StatusBadRequest)
		return
	}

	limit := 100 // значение по умолчанию
	if limitStr := query.Get("limit"); limitStr != "" {
		parsedLimit, err := strconv.Atoi(limitStr)
		if err != nil || parsedLimit < 1 {
			http.Error(w, "Invalid 'limit' parameter", http.StatusBadRequest)
			return
		}
		limit = parsedLimit
	}

	// 3. Сбор всех каналов, в которых состоит пользователь
	channels, appErr := p.getUserChannels(userID, sessionToken)
	if appErr != nil {
		p.API.LogError("Failed to get user channels", "error", appErr.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// 4. Получение сообщений из каждого канала после since
	var allPosts []*model.Post
	for _, ch := range channels {
		// GetPostsSince возвращает посты новее since в порядке убывания (от новых к старым)
		postList, appErr := p.API.GetPostsSince(ch.Id, since)
		if appErr != nil {
			p.API.LogError("Failed to get posts for channel", "channel_id", ch.Id, "error", appErr.Error())
			continue
		}
		// Добавляем все полученные посты
		for _, post := range postList.Posts {
			// Убедимся, что пост действительно после since (защита от возможных граничных случаев)
			if post.CreateAt > since {
				allPosts = append(allPosts, post)
			}
		}
	}

	// 5. Сортировка всех сообщений по возрастанию CreateAt (первые после отсечки)
	sort.Slice(allPosts, func(i, j int) bool {
		return allPosts[i].CreateAt < allPosts[j].CreateAt
	})

	// 6. Ограничение по лимиту
	if len(allPosts) > limit {
		allPosts = allPosts[:limit]
	}

	// 7. Ответ в JSON
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(allPosts); err != nil {
		p.API.LogError("Failed to encode response", "error", err.Error())
	}
}

// getUserChannels собирает все каналы пользователя (командные, открытые, приватные, прямые, групповые)
func (p *Plugin) getUserChannels(userID, token string) ([]*model.Channel, *model.AppError) {
	var allChannels []*model.Channel

	// Командные каналы
	teams, appErr := p.API.GetTeams()
	if appErr != nil {
		return nil, appErr
	}
	for _, team := range teams {
		channels, appErr := p.API.GetChannelsForTeamForUser(team.Id, userID, false)
		if appErr != nil {
			return nil, appErr
		}
		allChannels = append(allChannels, channels...)
	}

	// Личные каналы (прямые и групповые) через REST API, т.к. Plugin API 5.31 не имеет массовой выдачи
	// Выполняем GET /api/v4/users/{user_id}/channels – он возвращает и DM/GM
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", "http://localhost:8065/api/v4/users/"+userID+"/channels", nil) // адрес вашего Mattermost
	if err != nil {
		return nil, model.NewAppError("getUserChannels", "http_request_failed", nil, err.Error(), http.StatusInternalServerError)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, model.NewAppError("getUserChannels", "http_request_failed", nil, err.Error(), http.StatusInternalServerError)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, model.NewAppError("getUserChannels", "bad_response", nil, resp.Status, resp.StatusCode)
	}

	var channels []*model.Channel
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		return nil, model.NewAppError("getUserChannels", "json_decode_failed", nil, err.Error(), http.StatusInternalServerError)
	}

	// Добавляем только не дублирующиеся (вдруг командные попали снова)
	exist := make(map[string]bool)
	for _, ch := range allChannels {
		exist[ch.Id] = true
	}
	for _, ch := range channels {
		if !exist[ch.Id] {
			allChannels = append(allChannels, ch)
		}
	}

	return allChannels, nil
}
