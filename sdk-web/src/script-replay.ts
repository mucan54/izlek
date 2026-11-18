// <script> build of the replay module: exposes `window.izlekReplay`. Load after izlek.min.js.
import { replay, startReplay, stopReplay } from './replay'

;(globalThis as any).izlekReplay = { replay, startReplay, stopReplay }
