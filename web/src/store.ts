// Public surface of the client data layer for components and the WS router.
//
// State lives in Carburetor stores under ./state (one class + one instance per
// concern); this module re-exports the actions and pure helpers around them,
// which live one export per file in ./actions, ./liveWork and friends.
import "./actions/restoreCachedTheme";

export type { ModelRole } from "./state/ConfigCarburetor";
export type { QueuedMessage, WireAttachment, FlushedQueue } from "./state/QueueCarburetor";
export type { SessionLiveWork } from "./state/LiveWorkCarburetor";
export type { ExpandToolCallRequest } from "./state/SignalsCarburetor";
export type { MyPromptItem } from "./completionIndex";
export type { ConfigScope } from "./actions/ConfigScope";
export { mergePreserveContent } from "./mergeMessage";

export { trackModelUsage } from "./actions/trackModelUsage";
export { removeRecentModel } from "./actions/removeRecentModel";
export { removeSession } from "./actions/removeSession";
export { setActiveSession } from "./actions/setActiveSession";
export { tombstoneMessage } from "./actions/tombstoneMessage";
export { removeSessionQueueAndBusy } from "./actions/removeSessionQueueAndBusy";
export { removeSubAgentState } from "./actions/removeSubAgentState";
export { getDefaultModelKey } from "./actions/getDefaultModelKey";
export { updateTodos } from "./actions/updateTodos";
export { setSessionModel } from "./actions/setSessionModel";
export { clearSessionModelSlot } from "./actions/clearSessionModelSlot";
export { setSessionRoleEffort } from "./actions/setSessionRoleEffort";
export { applyTheme } from "./actions/applyTheme";
export { setTheme } from "./actions/setTheme";
export { jumpToMessage } from "./actions/jumpToMessage";

export { addContextPath } from "./actions/addContextPath";
export { removeContextPath } from "./actions/removeContextPath";
export { addSkillsPath } from "./actions/addSkillsPath";
export { removeSkillsPath } from "./actions/removeSkillsPath";
export { addCustomProvider } from "./actions/addCustomProvider";
export { removeCustomProvider } from "./actions/removeCustomProvider";
export { updateCustomProvider } from "./actions/updateCustomProvider";
export { setProviderPeakHours } from "./actions/setProviderPeakHours";
export { pauseProviderPeakHours } from "./actions/pauseProviderPeakHours";
export { resumeProviderPeakHours } from "./actions/resumeProviderPeakHours";
export { fetchPeakHoursStatus } from "./actions/fetchPeakHoursStatus";

export { setKeepAliveEnabled } from "./actions/setKeepAliveEnabled";
export { setProviderKey } from "./actions/setProviderKey";
export { removeProviderKey } from "./actions/removeProviderKey";
export { deleteMessage } from "./actions/deleteMessage";
export { deleteMessages } from "./actions/deleteMessages";
export { updateMessageContent } from "./actions/updateMessageContent";
export { updateMessageThinking } from "./actions/updateMessageThinking";
export { summarizeSession } from "./actions/summarizeSession";
export { cancelQueuedSummarize } from "./actions/cancelQueuedSummarize";
export { deleteMessagePart } from "./actions/deleteMessagePart";
export { updateMessagePart } from "./actions/updateMessagePart";
export { togglePinMessage } from "./actions/togglePinMessage";
export { rerunFromMessage } from "./actions/rerunFromMessage";
export { collectTurnContent } from "./actions/collectTurnContent";
export { sendWithFastModel } from "./actions/sendWithFastModel";
export { flushQueuedMessagesOnIdle } from "./actions/flushQueuedMessagesOnIdle";
export { sendQueuedMessageNow } from "./actions/sendQueuedMessageNow";
export { interruptAndSendQueuedMessage } from "./actions/interruptAndSendQueuedMessage";
export { sendQueuedMessageDirect } from "./actions/sendQueuedMessageDirect";

export { pickLiveWork } from "./liveWork/pickLiveWork";
export { sendGetSessionLiveWork } from "./liveWork/sendGetSessionLiveWork";
export { isLiveWorkRequestID } from "./liveWork/isLiveWorkRequestID";
export { pickWakeSchedules } from "./liveWork/pickWakeSchedules";
export { applyWakeSchedulesSnapshot } from "./liveWork/applyWakeSchedulesSnapshot";
export { sendGetSessionWakeSchedules } from "./liveWork/sendGetSessionWakeSchedules";
export { sendCancelWakeSchedule } from "./liveWork/sendCancelWakeSchedule";
export { isWakeSchedulesGetRequestID } from "./liveWork/isWakeSchedulesGetRequestID";
export { isWakeSchedulesCancelRequestID } from "./liveWork/isWakeSchedulesCancelRequestID";
export { pickPendingWake as pickPendingWakeSchedule } from "./wakeSnapshot/pickPendingWake";
