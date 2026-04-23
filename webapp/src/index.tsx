import {Store, Action} from 'redux';

import {GlobalState} from 'mattermost-redux/types/store';

import manifest from './manifest';

// eslint-disable-next-line import/no-unresolved
import {PluginRegistry} from './types/mattermost-webapp';

import CustomJsonSetting from './components/custom_json_setting';

/**
 * Расширяем интерфейс PluginRegistry, так как в старых версиях 
 * этот метод мог отсутствовать в определениях типов.
 */
interface ExtendedRegistry extends PluginRegistry {
    registerAdminConsoleCustomSetting(
        key: string,
        component: React.ComponentType<any>,
        options?: { showTitle?: boolean }
    ): void;
}

export default class Plugin {
    // eslint-disable-next-line @typescript-eslint/no-unused-vars, @typescript-eslint/no-empty-function
    public async initialize(registry: ExtendedRegistry, store: Store<GlobalState, Action<Record<string, unknown>>>) {
        // @see https://developers.mattermost.com/extend/plugins/webapp/reference/
        // ID 'Outgoing_Webhooks' должен строго совпадать с полем 'key' в plugin.json
        registry.registerAdminConsoleCustomSetting(
            'Outgoing_Webhooks',
            CustomJsonSetting,
            {showTitle: true}
        );
    }
}

declare global {
    interface Window {
        registerPlugin(id: string, plugin: Plugin): void
    }
}

window.registerPlugin(manifest.id, new Plugin());
