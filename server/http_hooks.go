package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/mattermost/mattermost-server/v5/model"
	"github.com/mattermost/mattermost-server/v5/plugin"
)

// PostWithMentions обёртка над model.Post, добавляющая email-упоминания.
type PostWithMentions struct {
	Post          *model.Post       `json:"post"`
	MentionEmails map[string]string `json:"mention_emails"` // username -> email, пустая если enrich_mentions=false
}

// ServeHTTP обрабатывает HTTP-запросы плагина
func (p *Plugin) ServeHTTP(c *plugin.Context, w http.ResponseWriter, r *http.Request) {
	p.router.ServeHTTP(w, r)
}

// initializeAPI инициализирует API для обработки HTTP-запросов
func (p *Plugin) initializeAPI() {
	router := mux.NewRouter()

	router.HandleFunc("/status", p.handleStatus).Methods("GET")
	router.HandleFunc("/{userId:[a-z0-9]{26}}/messages", p.handleUserMessages).Methods("GET")

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
func (p *Plugin) handleUserMessages(w http.ResponseWriter, r *http.Request) {
	// 1. Проверка авторизации
	userID, sessionToken := p.getUserAndSession(w, r)
	if userID == "" {
		return
	}

	// 2. Парсинг параметров запроса
	query := r.URL.Query()
	sinceStr := query.Get("since")
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

	enrichMentions := false
	if enrichStr := query.Get("enrich_mentions"); enrichStr == "true" || enrichStr == "1" {
		enrichMentions = true
	}

	filterUsername := query.Get("bot_username") // если пустая строка, фильтр не применяется
	filterByMention := filterUsername != ""

	// 3. Получение всех постов после since
	allPosts, appErr := p.getAllSortedPosts(userID, sessionToken, since)
	if appErr != nil {
		p.API.LogError("Failed to get posts", "error", appErr.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	selectedPosts := p.filterAndLimitPosts(allPosts, limit, filterByMention, filterUsername)

	// 4. Обогащение email-упоминаниями (только если enrich_mentions=true)
	result := make([]PostWithMentions, len(selectedPosts))
	if enrichMentions {
		result = p.enrichPostsEmailMentions(selectedPosts)
	}

	// 5. Ответ
	w.Header().Set("Content-Type", "application/json")
	if enrichMentions {
		if err := json.NewEncoder(w).Encode(result); err != nil {
			p.API.LogError("Failed to encode response", "error", err.Error())
		}
	} else {
		if err := json.NewEncoder(w).Encode(selectedPosts); err != nil {
			p.API.LogError("Failed to encode response", "error", err.Error())
		}
	}
}

// getUserAndSession Проверяет Bearer токен авторизации, возвращает ID пользователя и токен сессии
func (p *Plugin) getUserAndSession(w http.ResponseWriter, r *http.Request) (string, string) {
	vars := mux.Vars(r)
	requestUserID := vars["userId"]
	if requestUserID == "" {
		http.Error(w, "Missing userId in path", http.StatusBadRequest)
		return "", ""
	}

	token := r.Header.Get("Authorization")
	if len(token) < 7 || token[:7] != "Bearer " {
		http.Error(w, "Unauthorized: missing or malformed Bearer token", http.StatusUnauthorized)
		return "", ""
	}
	sessionToken := token[7:]
	session, err := p.API.GetSession(sessionToken)
	if err != nil || session == nil {
		p.API.LogError("Invalid session", "error", err.Error())
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return "", ""
	}

	// Получаем пользователя по токену
	currentUser, appErr := p.API.GetUser(session.UserId)
	if appErr != nil {
		p.API.LogError("Failed to get user from session", "error", appErr.Error())
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return "", ""
	}

	// Проверяем, является ли он администратором
	isAdmin := false
	for _, role := range strings.Fields(currentUser.Roles) {
		if role == model.SYSTEM_ADMIN_ROLE_ID {
			isAdmin = true
			break
		}
	}

	// Доступ только к своим сообщениям или у администраторов
	if !isAdmin && session.UserId != requestUserID {
		http.Error(w, "Forbidden: you can only request messages for your own user", http.StatusForbidden)
		return "", ""
	}

	return requestUserID, sessionToken
}

func (p *Plugin) getAllSortedPosts(userID string, sessionToken string, since int64) ([]*model.Post, *model.AppError) {
	var allPosts []*model.Post
	// Получение всех каналов пользователя
	channels, appErr := p.getUserChannels(userID, sessionToken)
	if appErr != nil {
		p.API.LogError("Failed to get user channels", "error", appErr.Error())
		return nil, appErr
	}

	// Сбор постов из каналов
	for _, ch := range channels {
		postList, appErr := p.API.GetPostsSince(ch.Id, since)
		if appErr != nil {
			p.API.LogError("Failed to get posts for channel", "channel_id", ch.Id, "error", appErr.Error())
			continue
		}
		for _, post := range postList.Posts {
			if post.CreateAt > since {
				allPosts = append(allPosts, post)
			}
		}
	}

	// Сортировка по возрастанию CreateAt (самые ранние после since)
	sort.Slice(allPosts, func(i, j int) bool {
		return allPosts[i].CreateAt < allPosts[j].CreateAt
	})

	return allPosts, nil
}

// filterAndLimitPosts отбирает сообщений с учётом фильтра по упоминаниям и лимита
func (p *Plugin) filterAndLimitPosts(allPosts []*model.Post, limit int, filterByMention bool, filterUsername string) []*model.Post {
	var selectedPosts []*model.Post
	if filterByMention {
		for _, post := range allPosts {
			// Проверяем упоминание
			mentions := model.PossibleAtMentions(post.Message)
			mentioned := false
			for _, m := range mentions {
				if m == filterUsername {
					mentioned = true
					break
				}
			}
			if mentioned {
				selectedPosts = append(selectedPosts, post)
				if len(selectedPosts) >= limit {
					break // Достигли лимита, останавливаем обход
				}
			}
		}
	} else {
		// Без фильтра – просто берём первые limit сообщений
		if len(allPosts) > limit {
			selectedPosts = allPosts[:limit]
		} else {
			selectedPosts = allPosts
		}
	}

	return selectedPosts
}

func (p *Plugin) enrichPostsEmailMentions(selectedPosts []*model.Post) []PostWithMentions {
	result := make([]PostWithMentions, len(selectedPosts))
	userCache := make(map[string]*model.User)
	for _, post := range selectedPosts {
		mentions := model.PossibleAtMentions(post.Message)
		mentionEmails := make(map[string]string)
		for _, username := range mentions {
			// Ищем в кэше
			user, ok := userCache[username]
			if !ok {
				var uErr *model.AppError
				user, uErr = p.API.GetUserByUsername(username)
				if uErr != nil {
					userCache[username] = nil
					continue
				}
				userCache[username] = user
			}
			if user == nil || user.IsBot {
				continue
			}
			// Проверка членства в канале
			_, memberErr := p.API.GetChannelMember(post.ChannelId, user.Id)
			if memberErr != nil {
				p.API.LogDebug("User mention is not a member of the channel",
					"channel_id", post.ChannelId,
					"username", username,
					"error", memberErr.Error())
				continue
			}
			mentionEmails[username] = user.Email
		}
		result = append(result, PostWithMentions{
			Post:          post,
			MentionEmails: mentionEmails,
		})
	}

	return result
}

// getUserChannels собирает все каналы пользователя (командные, открытые, приватные, прямые, групповые)
func (p *Plugin) getUserChannels(userID string, token string) ([]*model.Channel, *model.AppError) {
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
