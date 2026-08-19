export namespace main {
	
	export class ActiveRequest {
	    id: string;
	    account_id: string;
	    email: string;
	    label: string;
	    model: string;
	    started_at: string;
	    phase: string;
	
	    static createFrom(source: any = {}) {
	        return new ActiveRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.account_id = source["account_id"];
	        this.email = source["email"];
	        this.label = source["label"];
	        this.model = source["model"];
	        this.started_at = source["started_at"];
	        this.phase = source["phase"];
	    }
	}
	export class deviceLoginState {
	    login_id?: string;
	    device_code: string;
	    user_code: string;
	    verification_url: string;
	    interval: number;
	    expires_in: number;
	    started_at: string;
	
	    static createFrom(source: any = {}) {
	        return new deviceLoginState(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.login_id = source["login_id"];
	        this.device_code = source["device_code"];
	        this.user_code = source["user_code"];
	        this.verification_url = source["verification_url"];
	        this.interval = source["interval"];
	        this.expires_in = source["expires_in"];
	        this.started_at = source["started_at"];
	    }
	}

}

export namespace skills {
	
	export class Skill {
	    id: string;
	    name: string;
	    description: string;
	    path: string;
	    body?: string;
	    updated_at: string;
	
	    static createFrom(source: any = {}) {
	        return new Skill(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.path = source["path"];
	        this.body = source["body"];
	        this.updated_at = source["updated_at"];
	    }
	}

}

export namespace store {
	
	export class Settings {
	    active_account_id: string;
	    provider?: string;
	    default_model: string;
	    reasoning_effort: string;
	    api_mode: string;
	    upstream_base: string;
	    client_version: string;
	    proxy_listen: string;
	    proxy_enabled: boolean;
	    proxy_api_key?: string;
	    store_responses: boolean;
	    force_default_model?: boolean;
	    gemini_project?: string;
	    gemini_location?: string;
	    qwen_upstream?: string;
	    qwen_api_key?: string;
	    deepseek_api_key?: string;
	    opencode_go_api_key?: string;
	    theme_accent?: string;
	    kimi_stealth_headless: boolean;
	    google_email?: string;
	    google_password?: string;
	    load_balancer_strategies?: Record<string, string>;
	    system_prompts?: Record<string, any>;
	    auto_create_on_exhausted: boolean;
	    auto_create_min_accounts?: number;
	
	    static createFrom(source: any = {}) {
	        return new Settings(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.active_account_id = source["active_account_id"];
	        this.provider = source["provider"];
	        this.default_model = source["default_model"];
	        this.reasoning_effort = source["reasoning_effort"];
	        this.api_mode = source["api_mode"];
	        this.upstream_base = source["upstream_base"];
	        this.client_version = source["client_version"];
	        this.proxy_listen = source["proxy_listen"];
	        this.proxy_enabled = source["proxy_enabled"];
	        this.proxy_api_key = source["proxy_api_key"];
	        this.store_responses = source["store_responses"];
	        this.force_default_model = source["force_default_model"];
	        this.gemini_project = source["gemini_project"];
	        this.gemini_location = source["gemini_location"];
	        this.qwen_upstream = source["qwen_upstream"];
	        this.qwen_api_key = source["qwen_api_key"];
	        this.deepseek_api_key = source["deepseek_api_key"];
	        this.opencode_go_api_key = source["opencode_go_api_key"];
	        this.theme_accent = source["theme_accent"];
	        this.kimi_stealth_headless = source["kimi_stealth_headless"];
	        this.google_email = source["google_email"];
	        this.google_password = source["google_password"];
	        this.load_balancer_strategies = source["load_balancer_strategies"];
	        this.system_prompts = source["system_prompts"];
	        this.auto_create_on_exhausted = source["auto_create_on_exhausted"];
	        this.auto_create_min_accounts = source["auto_create_min_accounts"];
	    }
	}

}

export namespace upstream {
	
	export class ToolCall {
	    id: string;
	    type: string;
	    name: string;
	    arguments: string;
	
	    static createFrom(source: any = {}) {
	        return new ToolCall(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.type = source["type"];
	        this.name = source["name"];
	        this.arguments = source["arguments"];
	    }
	}
	export class ChatMessage {
	    role: string;
	    content: string;
	    name?: string;
	    tool_call_id?: string;
	    tool_calls?: ToolCall[];
	    reasoning_items?: any[];
	
	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.role = source["role"];
	        this.content = source["content"];
	        this.name = source["name"];
	        this.tool_call_id = source["tool_call_id"];
	        this.tool_calls = this.convertValues(source["tool_calls"], ToolCall);
	        this.reasoning_items = source["reasoning_items"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ChatRequest {
	    model: string;
	    messages: ChatMessage[];
	    input?: string;
	    stream: boolean;
	    reasoning_effort: string;
	    previous_response_id: string;
	    last_response_id: string;
	    api_mode: string;
	    temperature: number;
	    max_tokens: number;
	    web_search: boolean;
	    search_query?: string;
	
	    static createFrom(source: any = {}) {
	        return new ChatRequest(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.model = source["model"];
	        this.messages = this.convertValues(source["messages"], ChatMessage);
	        this.input = source["input"];
	        this.stream = source["stream"];
	        this.reasoning_effort = source["reasoning_effort"];
	        this.previous_response_id = source["previous_response_id"];
	        this.last_response_id = source["last_response_id"];
	        this.api_mode = source["api_mode"];
	        this.temperature = source["temperature"];
	        this.max_tokens = source["max_tokens"];
	        this.web_search = source["web_search"];
	        this.search_query = source["search_query"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class ModelInfo {
	    id: string;
	    name?: string;
	    description?: string;
	    api_mode?: string;
	    root?: string;
	    context_window?: number;
	    reasoning_efforts?: string[];
	    default_reasoning_effort?: string;
	    free_use?: boolean;
	    locked?: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ModelInfo(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.name = source["name"];
	        this.description = source["description"];
	        this.api_mode = source["api_mode"];
	        this.root = source["root"];
	        this.context_window = source["context_window"];
	        this.reasoning_efforts = source["reasoning_efforts"];
	        this.default_reasoning_effort = source["default_reasoning_effort"];
	        this.free_use = source["free_use"];
	        this.locked = source["locked"];
	    }
	}

}

