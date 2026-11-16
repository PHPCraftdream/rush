// Public surface of the client data layer for components and the WS router.
//
// State lives in Carburetor stores under ./state (one class + one instance per
// concern); this module re-exports the actions and pure helpers around them,
// which live one export per file in ./actions, ./liveWork and friends.
import "./theme/restoreCachedTheme";

export type { ModelRole } from "../state/config/ConfigCarburetor";
export type { QueuedMessage, WireAttachment, FlushedQueue } from "../state/transcript/QueueCarburetor";
export type { SessionLiveWork } from "../state/live/LiveWorkCarburetor";
export type { ExpandToolCallRequest } from "../state/status/SignalsCarburetor";
export type { MyPromptItem } from "../domain/completion/completionTypes";
export type { ConfigScope } from "./config/providers/ConfigScope";
export { mergePreserveContent } from "../domain/text/mergeMessage";

export { trackModelUsage } from "./sessions/models/trackModelUsage";
export { removeRecentModel } from "./sessions/models/removeRecentModel";
export { removeSession } from "./sessions/lifecycle/removeSession";
export { setActiveSession } from "./sessions/lifecycle/setActiveSession";
export { tombstoneMessage } from "./messages/nav/tombstoneMessage";
export { removeSessionQueueAndBusy } from "./sessions/lifecycle/removeSessionQueueAndBusy";
export { removeSubAgentState } from "./sessions/lifecycle/removeSubAgentState";
export { getDefaultModelKey } from "./sessions/models/getDefaultModelKey";
export { updateTodos } from "./sessions/lifecycle/updateTodos";
export { setSessionModel } from "./sessions/slots/setSessionModel";
export { clearSessionModelSlot } from "./sessions/slots/clearSessionModelSlot";
export { setSessionRoleEffort } from "./sessions/slots/setSessionRoleEffort";
export { applyTheme } from "./theme/applyTheme";
export { setTheme } from "./theme/setTheme";
export { jumpToMessage } from "./messages/nav/jumpToMessage";

export { addContextPath } from "./config/contexts/addContextPath";
export { removeContextPath } from "./config/contexts/removeContextPath";
export { addSkillsPath } from "./config/contexts/addSkillsPath";
export { removeSkillsPath } from "./config/contexts/removeSkillsPath";
export { addCustomProvider } from "./config/providers/addCustomProvider";
export { removeCustomProvider } from "./config/providers/removeCustomProvider";
export { updateCustomProvider } from "./config/providers/updateCustomProvider";
export { setProviderPeakHours } from "./config/peakHours/setProviderPeakHours";
export { pauseProviderPeakHours } from "./config/peakHours/pauseProviderPeakHours";
export { resumeProviderPeakHours } from "./config/peakHours/resumeProviderPeakHours";
export { fetchPeakHoursStatus } from "./config/peakHours/fetchPeakHoursStatus";

export { setKeepAliveEnabled } from "../backend/keepAlive/setKeepAliveEnabled";
export { setProviderKey } from "./config/providers/setProviderKey";
export { removeProviderKey } from "./config/providers/removeProviderKey";
export { deleteMessage } from "./messages/edit/deleteMessage";
export { deleteMessages } from "./messages/edit/deleteMessages";
export { updateMessageContent } from "./messages/edit/updateMessageContent";
export { updateMessageThinking } from "./messages/edit/updateMessageThinking";
export { summarizeSession } from "./sessions/lifecycle/summarizeSession";
export { cancelQueuedSummarize } from "./sessions/lifecycle/cancelQueuedSummarize";
export { deleteMessagePart } from "./messages/edit/deleteMessagePart";
export { updateMessagePart } from "./messages/edit/updateMessagePart";
export { togglePinMessage } from "./messages/nav/togglePinMessage";
export { rerunFromMessage } from "./messages/nav/rerunFromMessage";
export { collectTurnContent } from "./messages/nav/collectTurnContent";
export { sendWithFastModel } from "./sessions/models/sendWithFastModel";
export { flushQueuedMessagesOnIdle } from "./queue/flushQueuedMessagesOnIdle";
export { sendQueuedMessageNow } from "./queue/sendQueuedMessageNow";
export { interruptAndSendQueuedMessage } from "./queue/interruptAndSendQueuedMessage";
export { sendQueuedMessageDirect } from "./queue/sendQueuedMessageDirect";

export { pickLiveWork } from "./liveWork/transfer/pickLiveWork";
export { sendGetSessionLiveWork } from "./liveWork/transfer/sendGetSessionLiveWork";
export { isLiveWorkRequestID } from "./liveWork/protocol/isLiveWorkRequestID";
export { pickWakeSchedules } from "./liveWork/transfer/pickWakeSchedules";
export { applyWakeSchedulesSnapshot } from "./liveWork/transfer/applyWakeSchedulesSnapshot";
export { sendGetSessionWakeSchedules } from "./liveWork/transfer/sendGetSessionWakeSchedules";
export { sendCancelWakeSchedule } from "./liveWork/transfer/sendCancelWakeSchedule";
export { isWakeSchedulesGetRequestID } from "./liveWork/protocol/isWakeSchedulesGetRequestID";
export { isWakeSchedulesCancelRequestID } from "./liveWork/protocol/isWakeSchedulesCancelRequestID";
export { pickPendingWake as pickPendingWakeSchedule } from "./liveWork/wakeSnapshot/pickPendingWake";
