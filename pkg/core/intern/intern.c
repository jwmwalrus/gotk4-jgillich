#include "intern.h"

const gchar *gotk4_object_type_name(gpointer obj) {
  return G_OBJECT_TYPE_NAME(obj);
};

gboolean gotk4_intern_remove_toggle_ref(gpointer obj) {
  // Remove the toggle reference first. This may run GObject destruction
  // callbacks, so the Go intern entry is kept until this call returns.
  g_object_remove_toggle_ref(G_OBJECT(obj), (GToggleNotify)goToggleNotify,
                             NULL);

  // Complete the Go bookkeeping only after GLib has finished the C operation;
  // this also drops any plain reference transferred to the Box.
  goFinishRemovingToggleRef(obj);

  return FALSE;
}

void gotk4_intern_disconnect_closure(gpointer obj, GClosure *closure) {
  g_signal_handlers_disconnect_matched(obj, G_SIGNAL_MATCH_CLOSURE, 0, 0,
                                        closure, NULL, NULL);
}
