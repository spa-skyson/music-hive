import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import './demo.css'
import { Showcase } from './Showcase.tsx'

createRoot(document.getElementById('root')!).render(
  <StrictMode><Showcase /></StrictMode>,
)
