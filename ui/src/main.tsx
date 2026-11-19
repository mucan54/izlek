import { render } from 'preact'
import './styles.css'
import { App } from './app'
import { detectLocale, setLocale } from './i18n'

// before the first render: the stored choice, else the browser language
setLocale(detectLocale())

render(<App />, document.getElementById('app') as HTMLElement)
