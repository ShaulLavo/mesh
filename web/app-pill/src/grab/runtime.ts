export const nativeRequestAnimationFrame = window.requestAnimationFrame.bind(window);
export const nativeCancelAnimationFrame = window.cancelAnimationFrame.bind(window);
export const ignoreRealInput = <EventType,>(callback: (event: EventType) => void) => callback;
