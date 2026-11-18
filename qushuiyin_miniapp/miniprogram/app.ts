import { setEventForeground, watchEventNetwork } from './services/events'

App<IAppOption>({ globalData: {},
  onLaunch() { watchEventNetwork() },
  onShow() { setEventForeground(true) },
  onHide() { setEventForeground(false) },
})
