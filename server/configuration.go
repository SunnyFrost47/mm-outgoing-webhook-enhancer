package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"

	"github.com/pkg/errors"
)

// Структура configuration содержит внешнюю конфигурацию плагина, отображаемую в конфигурации сервера Mattermost,
// а также значения, вычисленные на основе этой конфигурации. Любые общедоступные поля будут десериализованы
// из конфигурации сервера Mattermost в методе OnConfigurationChange.
//
// Поскольку плагины по своей природе являются параллельными (хуки вызываются асинхронно),
// и конфигурация плагина может изменяться в любой момент, доступ к конфигурации должен быть синхронизирован.
// В этом плагине используется стратегия защиты указателя на конфигурацию и клонирования всей структуры при каждом её изменении.
type configuration struct {
	Debug        bool   `json:"debug"`
	JsonWebHooks string `json:"outgoing_webhooks"`

	// OutgoingWebhooks - массив кастомных исходящих вебхуков.
	OutgoingWebhooks []*CustomOutgoingWebhook
}

// CustomOutgoingWebhook определяет один кастомный исходящий вебхук.
type CustomOutgoingWebhook struct {
	// DisplayName - отображаемое имя (для админки).
	DisplayName string `json:"display_name"`
	// Enabled - активен ли вебхук.
	Enabled bool `json:"enabled"`
	// TriggerWords - список слов-триггеров.
	TriggerWords []string `json:"trigger_words"`
	// TriggerWhen - условие срабатывания: "startswith", "exact", "regex".
	TriggerWhen string `json:"trigger_when"`
	// CallbackURLs - список конечных точек, куда будет отправлен POST-запрос.
	CallbackURLs []string `json:"callback_urls"`
	// ContentType - тип содержимого запроса ("application/json" или "application/x-www-form-urlencoded").
	ContentType string `json:"content_type"`
	// Secret - секретный токен для подписи запроса (опционально).
	Token string `json:"secret"`
	// ChannelIDs - ограничение каналов (пустой массив = все каналы).
	ChannelIDs []string `json:"channel_ids"`
	// Enabled - активен ли вебхук.
	CheckBotAccess bool `json:"check_bot_access"`
}

// Validate проверяет обязательные поля и корректность структуры.
func (h *CustomOutgoingWebhook) Validate() error {
	if h.DisplayName == "" {
		return errors.New("display name is required")
	}
	if len(h.TriggerWords) == 0 && len(h.ChannelIDs) == 0 {
		return errors.New("at least one trigger word or channel is required")
	}
	if len(h.CallbackURLs) == 0 {
		return errors.New("at least one callback_urls is required")
	}
	if h.TriggerWhen != "" && h.TriggerWhen != "startswith" && h.TriggerWhen != "exact" && h.TriggerWhen != "regex" {
		return errors.New("trigger_when must be one of: startswith, exact, regex")
	}
	if h.ContentType != "" && h.ContentType != "application/json" && h.ContentType != "application/x-www-form-urlencoded" {
		return errors.New("content_type must be application/json or application/x-www-form-urlencoded")
	}
	return nil
}

// Clone возвращает глубокую копию конфигурации.
func (c *configuration) Clone() *configuration {
	var clone configuration
	data, _ := json.Marshal(c)
	_ = json.Unmarshal(data, &clone)
	return &clone
}

// getConfiguration извлекает текущую конфигурацию из хранилища плагина.
// Используется sync.RWMutex для потокобезопасности.
func (p *Plugin) getConfiguration() *configuration {
	p.configurationLock.RLock()
	defer p.configurationLock.RUnlock()

	if p.configuration == nil {
		return &configuration{}
	}

	return p.configuration
}

// setConfiguration replaces the active configuration under lock.
//
// Do not call setConfiguration while holding the configurationLock, as sync.Mutex is not
// reentrant. In particular, avoid using the plugin API entirely, as this may in turn trigger a
// hook back into the plugin. If that hook attempts to acquire this lock, a deadlock may occur.
//
// This method panics if setConfiguration is called with the existing configuration. This almost
// certainly means that the configuration was modified without being cloned and may result in
// an unsafe access.
func (p *Plugin) setConfiguration(configuration *configuration) error {
	p.configurationLock.Lock()
	defer p.configurationLock.Unlock()

	if configuration != nil && p.configuration == configuration {
		// Ignore assignment if the configuration struct is empty. Go will optimize the
		// allocation for same to point at the same memory address, breaking the check
		// above.
		if reflect.ValueOf(*configuration).NumField() == 0 {
			return nil
		}

		panic("setConfiguration called with the existing configuration")
	}

	// Валидация всех вебхуков перед сохранением.
	for _, wh := range configuration.OutgoingWebhooks {
		if err := wh.Validate(); err != nil {
			return errors.Wrapf(err, "invalid webhook %s", wh.DisplayName)
		}
	}

	p.configuration = configuration
	return nil
}

// OnConfigurationChange обрабатывает событие изменения конфигурации.
func (p *Plugin) OnConfigurationChange() error {
	if p.API == nil {
		// Во время тестирования API может быть равен нулю.
		fmt.Fprintf(os.Stderr, "ERROR: OnConfigurationChange called but p.API is nil\n")
		return errors.New("API is nil")
	}
	var configuration = new(configuration)

	// Load the public configuration fields from the Mattermost server configuration.
	if err := p.API.LoadPluginConfiguration(configuration); err != nil {
		return errors.Wrap(err, "failed to load plugin configuration")
	}

	if configuration.JsonWebHooks == "" {
		configuration.OutgoingWebhooks = []*CustomOutgoingWebhook{}
	} else {
		if err := json.Unmarshal([]byte(configuration.JsonWebHooks), &configuration.OutgoingWebhooks); err != nil {
			// Логируем ошибку, чтобы администратор видел её в логах Mattermost
			p.API.LogError("Failed to parse OutgoingWebhooks JSON setting", "error", err.Error())
			return errors.Wrap(err, "invalid JSON format in settings")
		}
	}

	// Логируем успешную загрузку конфигурации.
	p.API.LogInfo("Configuration loaded", "webhooks_count", len(configuration.OutgoingWebhooks))

	// Дополнительно: вывести первый вебхук, если есть
	if len(configuration.OutgoingWebhooks) > 0 {
		p.API.LogDebug("First webhook",
			"id", configuration.OutgoingWebhooks[0].DisplayName,
			"secret", configuration.OutgoingWebhooks[0].Token) // убедитесь, что Secret не пуст
	}

	// Сохраняем конфигурацию в плагине (с валидацией).
	if err := p.setConfiguration(configuration); err != nil {
		return err
	}

	return nil
}
