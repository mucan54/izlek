// <script> build of the core: exposes `window.izlek`.
import { SDK, client, create } from './index'

;(globalThis as any).izlek = { ...client, client, create, SDK }
