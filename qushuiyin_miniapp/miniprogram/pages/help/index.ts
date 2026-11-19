import { loadConfig } from '../../services/api'

Page({
  data: { message: '', mock: false },
  async onShow() {
    try {
      const config = await loadConfig()
      this.setData({ message: config.help?.message || '', mock: config.features.mock })
    } catch { /* General help remains available offline. */ }
  },
})
