import {createApp} from 'vue'
import App from './App.vue'
import {installBrowserBridge} from './browserBridge'
import './style.css';
import 'bootstrap-icons/font/bootstrap-icons.css';

// Opened in a browser instead of the Wails window, this swaps the Go bindings
// for token-gated calls to the console control plane.
installBrowserBridge()

createApp(App).mount('#app')
