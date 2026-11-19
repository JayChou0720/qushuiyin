import { setEventForeground, watchEventNetwork } from './services/events'
import { watchSavePrivacy } from './services/save-media'

App<IAppOption>({ globalData: {},
  onLaunch() { watchEventNetwork(); watchSavePrivacy() },
  onShow() { setEventForeground(true) },
  onHide() { setEventForeground(false) },
})
