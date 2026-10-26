import DefaultTheme from 'vitepress/theme'
import CopyBox from './CopyBox.vue'
import './style.css'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('CopyBox', CopyBox)
  }
}
