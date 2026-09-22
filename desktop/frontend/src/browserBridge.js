// Wails injects window.go / window.runtime into the desktop webview. Served to
// a plain browser neither exists, so this installs HTTP equivalents over the
// console control plane: every binding becomes a token-gated fetch and every
// Go->JS event becomes one shared EventSource. The Vue views stay untouched.

const TOKEN_KEY = 'lingmaConsoleToken'

const reads = {
  GetAppVersion: 'version',
  GetStatus: 'status',
  GetConfig: 'config',
  GetDetectionInfo: 'detection',
  GetModels: 'models',
  GetTokenStats: 'stats',
  GetRequestSummaries: 'requests',
  GetLogSummaries: 'logs',
  GetRequestDetail: 'requests/detail',
  GetLogDetail: 'logs/detail',
  ConsoleInfo: 'console',
}

const writes = {
  UpdateConfig: ['config', (args) => args[0]],
  SelectModel: ['models/select', (args) => ({ model: args[0] })],
  RefreshModels: ['models/refresh', () => ({})],
  ClearRequests: ['requests/clear', () => ({})],
  ClearLogs: ['logs/clear', () => ({})],
  StartProxy: ['proxy/start', () => ({})],
  StopProxy: ['proxy/stop', () => ({})],
  RestartProxy: ['proxy/restart', () => ({})],
}

// desktopOnly has no browser meaning: native dialogs, file paths and windows.
const desktopOnly = [
  'ChooseFeedbackExportPath',
  'ExportFeedbackBundle',
  'ExportServerDeploymentBundle',
  'OpenPathInFileManager',
  'ShowWindow',
  'HideWindow',
  'MinimizeWindow',
  'ForceQuitApp',
  'RequestQuitShortcut',
]

function tokenFromUrl() {
  const match = /[?&#]token=([^&#]+)/.exec(window.location.href)
  return match ? decodeURIComponent(match[1]) : ''
}

function consoleToken() {
  let token = tokenFromUrl() || sessionStorage.getItem(TOKEN_KEY) || ''
  if (!token) {
    token = window.prompt('Web 控制台令牌（桌面版：设置页 -> 控制台令牌）') || ''
    token = token.trim()
  }
  if (token) sessionStorage.setItem(TOKEN_KEY, token)
  return token
}

async function request(path, body) {
  const headers = { Authorization: 'Bearer ' + consoleToken() }
  const init = body === undefined ? { headers } : {
    method: 'POST',
    headers: Object.assign({'Content-Type': 'application/json'}, headers),
    body: JSON.stringify(body),
  }
  const response = await fetch('/api/admin/' + path, init)
  const text = await response.text()
  const payload = text ? JSON.parse(text) : null
  if (!response.ok) {
    throw new Error((payload && payload.error) || `HTTP ${response.status}`)
  }
  return payload
}

function bindings() {
  const app = {}
  for (const [method, path] of Object.entries(reads)) {
    app[method] = (...args) => {
      const query = args[0] ? '?id=' + encodeURIComponent(args[0]) : ''
      return request(path + query)
    }
  }
  for (const [method, [path, build]] of Object.entries(writes)) {
    app[method] = (...args) => request(path, build(args))
  }
  for (const method of desktopOnly) {
    app[method] = () => Promise.reject(new Error(`${method} 只在桌面版可用`))
  }
  return app
}

const listeners = new Map()
let stream = null

function dispatch(event) {
  const handlers = listeners.get(event.name)
  if (!handlers) return
  for (const handler of handlers) handler(event.data)
}

function connect() {
  if (stream) return
  stream = new EventSource('/api/admin/events?token=' + encodeURIComponent(consoleToken()))
  stream.onmessage = (message) => {
    try {
      dispatch(JSON.parse(message.data))
    } catch (error) {
      console.debug('bad console event', error)
    }
  }
  // EventSource retries on its own; nothing to do but wait for the next open.
  stream.onerror = () => {}
}

function events() {
  return {
    EventsOnMultiple: (name, callback) => {
      if (!listeners.has(name)) listeners.set(name, new Set())
      listeners.get(name).add(callback)
      connect()
      return () => events().EventsOff(name)
    },
    EventsOff: (name) => {
      listeners.delete(name)
      if (listeners.size === 0 && stream) {
        stream.close()
        stream = null
      }
    },
    EventsEmit: () => {},
    LogPrint: (message) => console.log(message),
    LogInfo: (message) => console.info(message),
    LogError: (message) => console.error(message),
    ClipboardSetText: (text) => navigator.clipboard.writeText(text).then(() => true),
    BrowserOpenURL: (url) => {
      window.open(url, '_blank')
      return Promise.resolve()
    },
  }
}

export function installBrowserBridge() {
  if (window.go && window.runtime) return false
  window.go = { main: { App: bindings() } }
  window.runtime = events()
  return true
}
